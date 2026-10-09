"""Embed the Agen agent engine in Python.

    from agen import Agent, tool, openrouter

    @tool(read_only=True)
    def add(a: int, b: int) -> int:
        "Add two integers."
        return a + b

    agent = Agent(name="calc", instructions="Use tools for arithmetic.",
                  model="deepseek/deepseek-v4-flash", provider=openrouter(), tools=[add])
    result = agent.run("What is 2+3?", on_delta=lambda s: print(s, end=""))
    print(result.output)

The engine is the same Rust engine the Agen platform runs.
"""

from __future__ import annotations

import asyncio
import inspect
import itertools
import json
import threading
import typing
import weakref
from dataclasses import dataclass, field
from typing import Any, Callable, Optional

from ._native import AgenError as _NativeError
from ._native import NativeAgent, __version__

__all__ = [
    "Agent",
    "AgenError",
    "RunResult",
    "Usage",
    "Tool",
    "tool",
    "fake",
    "openrouter",
    "openai",
    "__version__",
]


class AgenError(Exception):
    """An engine error with a stable ``code``."""

    def __init__(self, code: str, message: str):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message

    @classmethod
    def _from_native(cls, e: Exception) -> "AgenError":
        try:
            d = json.loads(e.args[0])
            return cls(d.get("code", "internal"), d.get("message", ""))
        except Exception:
            return cls("internal", str(e))


# ---- providers ----


def fake(*responses: dict, cycle: bool = False) -> dict:
    """Scripted responses, e.g. ``{"text": "hi"}`` or
    ``{"toolCalls": [{"name": "add", "arguments": {"a": 1, "b": 2}}]}``."""
    return {"type": "fake", "responses": list(responses), "cycle": cycle}


def openrouter(api_key: Optional[str] = None, base_url: Optional[str] = None) -> dict:
    """OpenRouter. Without ``api_key`` reads ``$OPENROUTER_API_KEY``."""
    p: dict = {"type": "openrouter"}
    if api_key:
        p["apiKey"] = api_key
    if base_url:
        p["baseUrl"] = base_url
    return p


def openai(api_key: Optional[str] = None, base_url: Optional[str] = None) -> dict:
    """OpenAI directly. Without ``api_key`` reads ``$OPENAI_API_KEY``."""
    p: dict = {"type": "openai"}
    if api_key:
        p["apiKey"] = api_key
    if base_url:
        p["baseUrl"] = base_url
    return p


# ---- tools ----

_JSON_TYPES = {str: "string", int: "integer", float: "number", bool: "boolean", list: "array", dict: "object"}


def _schema_for(fn: Callable) -> dict:
    sig = inspect.signature(fn)
    hints = typing.get_type_hints(fn)
    props, required = {}, []
    for name, p in sig.parameters.items():
        if p.kind in (p.VAR_POSITIONAL, p.VAR_KEYWORD) or name == "cancel":
            continue
        t = hints.get(name)
        origin = typing.get_origin(t) or t
        props[name] = {"type": _JSON_TYPES[origin]} if origin in _JSON_TYPES else {}
        if p.default is p.empty:
            required.append(name)
    return {"type": "object", "properties": props, "required": required}


@dataclass
class Tool:
    """A tool implemented in Python (sync or ``async def``)."""

    fn: Callable[..., Any]
    name: str
    description: str = ""
    parameters: dict = field(default_factory=lambda: {"type": "object"})
    read_only: bool = False
    timeout_seconds: Optional[int] = None

    def _spec(self) -> dict:
        s = {"name": self.name, "description": self.description, "parameters": self.parameters, "sideEffect": not self.read_only}
        if self.timeout_seconds:
            s["timeoutSeconds"] = self.timeout_seconds
        return s


def tool(fn: Optional[Callable] = None, *, name: Optional[str] = None, description: Optional[str] = None,
         read_only: bool = False, timeout_seconds: Optional[int] = None, parameters: Optional[dict] = None):
    """Turn a function into a :class:`Tool`. The JSON Schema comes from type
    hints unless ``parameters`` is given; the description from the docstring.
    Tools are side-effecting unless ``read_only=True``. A parameter named
    ``cancel`` receives a ``threading.Event`` that is set if the run is
    cancelled or the call is abandoned."""

    def wrap(f: Callable) -> Tool:
        return Tool(
            fn=f,
            name=name or f.__name__,
            description=description or inspect.getdoc(f) or "",
            parameters=parameters or _schema_for(f),
            read_only=read_only,
            timeout_seconds=timeout_seconds,
        )

    return wrap(fn) if fn is not None else wrap


# ---- results ----


@dataclass
class Usage:
    input_tokens: int = 0
    output_tokens: int = 0
    cost_usd: float = 0.0


@dataclass
class RunResult:
    run_id: str
    session_id: str
    conversation_id: str
    status: str  # "succeeded" | "failed" | "cancelled"
    output: str
    error: str
    usage: Usage
    trace_id: str

    @property
    def ok(self) -> bool:
        return self.status == "succeeded"

    @classmethod
    def _from_json(cls, s: str) -> "RunResult":
        d = json.loads(s)
        u = d.get("usage") or {}
        return cls(
            run_id=d["runId"],
            session_id=d["sessionId"],
            conversation_id=d["conversationId"],
            status=d["status"],
            output=d["output"],
            error=d["error"],
            usage=Usage(u.get("inputTokens", 0), u.get("outputTokens", 0), u.get("costUsd", 0.0)),
            trace_id=d["traceId"],
        )


# ---- agent ----

_keys = itertools.count(1)


def _make_host(agent: "Agent") -> Callable[[str], None]:
    # Only a weak reference: the native agent holds this callback, and a strong
    # reference back to the Agent would be a cycle the GC cannot collect.
    ref = weakref.ref(agent)

    def host(request_json: str) -> None:
        a = ref()
        if a is not None:
            a._on_host(request_json)

    return host


class Agent:
    """An embedded agent. Thread-safe; ``run`` releases the GIL."""

    def __init__(
        self,
        name: Optional[str] = None,
        *,
        bundle: Optional[str] = None,
        instructions: Optional[str] = None,
        model: Optional[str] = None,
        provider: Optional[dict] = None,
        tools: typing.Sequence[Tool] = (),
        approve: Optional[Callable[[str, dict], bool]] = None,
        store: Optional[str] = None,
        secrets: Optional[dict] = None,
        permissions: Optional[dict] = None,
        max_turns: Optional[int] = None,
        max_output_tokens: Optional[int] = None,
        temperature: Optional[float] = None,
        approval_timeout_seconds: Optional[int] = None,
        namespace: Optional[str] = None,
        deployment: Optional[str] = None,
    ):
        self._tools = {t.name: t for t in tools}
        self._approve = approve
        self._closed = False
        self._cancels: dict = {}  # request id -> threading.Event
        self._cancels_lock = threading.Lock()
        self._loops: dict = {}  # event loop -> number of arun calls in flight on it
        spec = {
            k: v
            for k, v in {
                "bundle": bundle,
                "name": name,
                "instructions": instructions,
                "model": model,
                "provider": provider,
                "store": store,
                "secrets": secrets,
                "permissions": permissions,
                "maxTurns": max_turns,
                "maxOutputTokens": max_output_tokens,
                "temperature": temperature,
                "approvalTimeoutSeconds": approval_timeout_seconds,
                "namespace": namespace,
                "deployment": deployment,
            }.items()
            if v is not None
        }
        spec["tools"] = [t._spec() for t in tools]
        spec["hostApprovals"] = approve is not None
        try:
            self._native = NativeAgent(json.dumps(spec), _make_host(self))
        except _NativeError as e:
            raise AgenError._from_native(e) from None

    # Called on the engine's dispatcher thread: hand off and return at once.
    # Every request must be answered, or the run would wait forever.
    def _on_host(self, request_json: str) -> None:
        rid = None
        try:
            req = json.loads(request_json)
            rid = req["id"]
            if req.get("kind") == "cancel":
                with self._cancels_lock:
                    ev = self._cancels.get(rid)
                if ev is not None:
                    ev.set()
                return
            ev = threading.Event()
            with self._cancels_lock:
                self._cancels[rid] = ev
            # One thread per request: a tool may itself call run(), so a fixed
            # pool could starve. Daemon threads never block interpreter exit.
            threading.Thread(target=self._serve, args=(req, ev), daemon=True, name="agen-host").start()
        except BaseException as e:  # e.g. thread creation failed
            if rid is not None:
                with self._cancels_lock:
                    self._cancels.pop(rid, None)
                self._native.complete(rid, f"{type(e).__name__}: {e}", True)

    def _serve(self, req: dict, cancel: threading.Event) -> None:
        rid = req["id"]
        try:
            if req["kind"] == "tool":
                t = self._tools.get(req["name"])
                if t is None:
                    self._native.complete(rid, f"no Python implementation for tool {req['name']}", True)
                    return
                args = req.get("arguments") or {}
                if not isinstance(args, dict):
                    raise TypeError("tool arguments must be a JSON object")
                if "cancel" in inspect.signature(t.fn).parameters:
                    args = {**args, "cancel": cancel}
                out = t.fn(**args)
                if inspect.isawaitable(out):
                    loops = [lp for lp in self._loops if lp.is_running()]
                    # Only unambiguous when a single event loop is driving arun.
                    loop = loops[0] if len(loops) == 1 else None
                    if loop is not None:
                        out = asyncio.run_coroutine_threadsafe(_await(out), loop).result()
                    else:
                        out = asyncio.run(_await(out))
                if not isinstance(out, str):
                    # Compact, like every other SDK. TypeError for non-JSON -> tool error.
                    out = json.dumps(out, separators=(",", ":"))
                self._native.complete(rid, out, False)
            elif req["kind"] == "approval":
                ok = bool(self._approve and self._approve(req.get("tool", ""), req.get("arguments") or {}))
                self._native.complete(rid, "approved" if ok else "denied", False)
        except BaseException as e:  # tool errors (even CancelledError) go back to the model
            self._native.complete(rid, f"{type(e).__name__}: {e}", True)
        finally:
            with self._cancels_lock:
                self._cancels.pop(rid, None)

    def run(
        self,
        input: str,
        *,
        on_delta: Optional[Callable[[str], None]] = None,
        on_reset: Optional[Callable[[], None]] = None,
        session_id: Optional[str] = None,
        task_id: Optional[str] = None,
        new_conversation: bool = False,
        traceparent: Optional[str] = None,
        cancel: Optional["CancelScope"] = None,
    ) -> RunResult:
        """Run to completion (blocking; other Python threads keep running)."""
        if self._closed:
            raise AgenError("closed", "agent is closed")
        opts: dict = {}
        if session_id:
            opts["sessionId"] = session_id
        if task_id:
            opts["taskId"] = task_id
        if new_conversation:
            opts["newConversation"] = True
        if traceparent:
            opts["traceparent"] = traceparent
        key = f"py-{next(_keys)}"
        opts["cancelKey"] = key
        if cancel is not None:
            cancel._bind(self._native, key)

        def on_event(ev_json: str) -> None:
            ev = json.loads(ev_json)
            if ev["type"] == "delta" and on_delta:
                on_delta(ev["text"])
            elif ev["type"] == "reset" and on_reset:
                on_reset()

        try:
            out = self._native.run(input, json.dumps(opts), on_event)
        except _NativeError as e:
            raise AgenError._from_native(e) from None
        return RunResult._from_json(out)

    def trace(self, trace_id: str) -> list:
        """Spans of a run's trace (``result.trace_id``): dicts with spanId,
        parentSpanId, name, runId, startMs, endMs, status, attributes."""
        try:
            return json.loads(self._native.trace(trace_id))
        except _NativeError as e:
            raise AgenError._from_native(e) from None

    async def arun(self, input: str, **kwargs: Any) -> RunResult:
        """Async ``run``. Cancelling the awaiting task cancels the run.
        ``async def`` tools then run on this event loop."""
        scope = CancelScope()
        loop = asyncio.get_running_loop()
        self._loops[loop] = self._loops.get(loop, 0) + 1
        fut = loop.run_in_executor(None, lambda: self.run(input, cancel=scope, **kwargs))
        try:
            return await asyncio.shield(fut)
        except asyncio.CancelledError:
            scope.cancel()
            await fut
            raise
        finally:
            self._loops[loop] -= 1
            if not self._loops[loop]:
                del self._loops[loop]

    def close(self) -> None:
        """Release the engine (store, MCP servers). Idempotent."""
        if self._closed:
            return
        self._closed = True
        with self._cancels_lock:
            for ev in self._cancels.values():
                ev.set()
        self._native.close()

    def __enter__(self) -> "Agent":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()


class CancelScope:
    """Cancel a run from another thread: ``scope.cancel()``. Safe to call
    before the run has started (it then starts cancelled)."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._native: Any = None
        self._key: Optional[str] = None
        self._cancelled = False

    def _bind(self, native: Any, key: str) -> None:
        with self._lock:
            self._native, self._key = native, key
            if self._cancelled:
                native.cancel(key)

    def cancel(self) -> None:
        with self._lock:
            self._cancelled = True
            if self._native is not None:
                self._native.cancel(self._key)


async def _await(x: Any) -> Any:
    return await x


__all__.append("CancelScope")

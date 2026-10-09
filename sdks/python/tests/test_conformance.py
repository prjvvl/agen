"""Shared SDK conformance suite (spec/conformance/cases) for the Python SDK."""

import concurrent.futures
import glob
import json
import os
import threading
import time

import pytest

from agen import Agent, AgenError, CancelScope, Tool

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CASES = sorted(glob.glob(os.path.join(ROOT, "spec", "conformance", "cases", "*.json")))

CASE_KEYS = {"name", "description", "agent", "tools", "approvals", "runs", "expect", "createError"}
AGENT_KEYS = {
    "name": "name",
    "bundle": "bundle",
    "instructions": "instructions",
    "model": "model",
    "provider": "provider",
    "permissions": "permissions",
    "maxTurns": "max_turns",
    "approvalTimeoutSeconds": "approval_timeout_seconds",
}
TOOL_KEYS = {"name", "readOnly", "timeoutSeconds", "behavior"}
RUN_KEYS = {"input", "options", "sameSession", "cancelAfterMs", "cancelBeforeStart", "closeAfterMs", "concurrent"}
OPTION_KEYS = {"taskId"}
EXPECT_KEYS = {
    "status", "output", "minDeltas", "deltasEqualOutput", "toolCalls", "approvalsAsked", "toolCancelled",
    "sameConversationAsPrevious", "sameRunAsPrevious", "maxWallMs", "traceIncludes",
}


def strict(d: dict, allowed, where: str):
    unknown = set(d) - set(allowed)
    assert not unknown, f"{where}: unknown keys {sorted(unknown)} (update the runner)"


class State:
    def __init__(self):
        self.lock = threading.Lock()
        self.calls = []
        self.approvals = []
        self.tool_cancelled = threading.Event()


def make_tool(spec: dict, state: State) -> Tool:
    strict(spec, TOOL_KEYS, "tool")
    b = spec["behavior"]

    def fn(cancel: threading.Event, **args):
        with state.lock:
            state.calls.append(spec["name"])
        if "return" in b:
            return b["return"]
        if b.get("echoArgs"):
            return json.dumps(args, separators=(",", ":"))
        if "error" in b:
            raise RuntimeError(b["error"])
        if "sleepMs" in b:
            if cancel.wait(b["sleepMs"] / 1000):
                state.tool_cancelled.set()
            return "slept"
        if b.get("nonJson"):
            return object()
        raise AssertionError(f"unknown behavior {b}")

    return Tool(
        fn=fn,
        name=spec["name"],
        parameters={"type": "object"},
        read_only=spec.get("readOnly", False),
        timeout_seconds=spec.get("timeoutSeconds"),
    )


def agent_kwargs(spec: dict) -> dict:
    strict(spec, AGENT_KEYS, "agent")
    out = {AGENT_KEYS[k]: v for k, v in spec.items()}
    if "bundle" in out:
        out["bundle"] = os.path.join(ROOT, out["bundle"])
    return out


def run_once(agent: Agent, run: dict, prev):
    deltas = []
    kw = {}
    opts = run.get("options", {})
    strict(opts, OPTION_KEYS, "options")
    if "taskId" in opts:
        kw["task_id"] = opts["taskId"]
    if run.get("sameSession"):
        kw["session_id"] = prev["sessionId"]
    scope = CancelScope()
    if run.get("cancelBeforeStart"):
        scope.cancel()
    if "cancelAfterMs" in run:
        threading.Timer(run["cancelAfterMs"] / 1000, scope.cancel).start()
    if "closeAfterMs" in run:
        threading.Timer(run["closeAfterMs"] / 1000, agent.close).start()
    r = agent.run(run["input"], on_delta=deltas.append, on_reset=deltas.clear, cancel=scope, **kw)
    return {"status": r.status, "output": r.output, "runId": r.run_id, "sessionId": r.session_id,
            "conversationId": r.conversation_id, "traceId": r.trace_id, "deltas": deltas}


def check(agent, exp, got, prev, state, calls_before, approvals_before, single):
    assert got["status"] == exp["status"], got
    if "output" in exp:
        assert got["output"] == exp["output"], got
    if "minDeltas" in exp:
        assert len(got["deltas"]) >= exp["minDeltas"], got["deltas"]
    if exp.get("deltasEqualOutput"):
        assert "".join(got["deltas"]) == got["output"]
    if "toolCalls" in exp and single:
        assert state.calls[calls_before:] == exp["toolCalls"]
    if "approvalsAsked" in exp:
        assert state.approvals[approvals_before:] == exp["approvalsAsked"]
    if exp.get("toolCancelled"):
        assert state.tool_cancelled.wait(3), "tool never saw the cancel"
    if exp.get("sameConversationAsPrevious"):
        assert got["conversationId"] == prev["conversationId"]
    if exp.get("sameRunAsPrevious"):
        assert got["runId"] == prev["runId"]
    if "traceIncludes" in exp:
        names = {s["name"] for s in agent.trace(got["traceId"])}
        missing = set(exp["traceIncludes"]) - names
        assert not missing, f"trace lacks {missing}: {names}"


@pytest.mark.parametrize("path", CASES, ids=[os.path.basename(p)[:-5] for p in CASES])
def test_conformance(path):
    case = json.load(open(path, encoding="utf-8"))
    strict(case, CASE_KEYS, "case")
    state = State()
    tools = [make_tool(t, state) for t in case.get("tools", [])]
    approve = None
    mode = case.get("approvals")
    assert mode in (None, "approve", "deny", "hang"), mode
    if mode is not None:

        def approve(tool, args):
            with state.lock:
                state.approvals.append(tool)
            if mode == "hang":
                time.sleep(5)  # never answers in time
            return mode == "approve"

    if "createError" in case:
        with pytest.raises(AgenError) as e:
            Agent(**agent_kwargs(case["agent"]), tools=tools, approve=approve)
        assert e.value.code == case["createError"]
        return

    agent = Agent(**agent_kwargs(case["agent"]), tools=tools, approve=approve)
    prev = None
    try:
        assert len(case["runs"]) == len(case["expect"])
        for run, exp in zip(case["runs"], case["expect"]):
            strict(run, RUN_KEYS, "run")
            strict(exp, EXPECT_KEYS, "expect")
            n = run.get("concurrent", 1)
            calls_before, approvals_before = len(state.calls), len(state.approvals)
            started = time.monotonic()
            with concurrent.futures.ThreadPoolExecutor(n) as pool:
                results = list(pool.map(lambda _: run_once(agent, run, prev), range(n)))
            wall_ms = (time.monotonic() - started) * 1000
            if "maxWallMs" in exp:
                assert wall_ms <= exp["maxWallMs"], f"took {wall_ms:.0f} ms > {exp['maxWallMs']} ms"
            for got in results:
                check(agent, exp, got, prev, state, calls_before, approvals_before, n == 1)
            prev = results[-1]
    finally:
        agent.close()

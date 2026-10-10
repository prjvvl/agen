import asyncio
import os
import threading
import time

import pytest

from agen import Agent, AgenError, CancelScope, fake, tool

HELLO = os.path.join(os.path.dirname(__file__), "..", "..", "..", "examples", "bundles", "hello")


def call(name, **args):
    return {"toolCalls": [{"name": name, "arguments": args}]}


def say(text, expect=None, **kw):
    d = {"text": text, **kw}
    if expect:
        d["expect"] = expect
    return d


@tool(read_only=True)
def add(a: int, b: int) -> int:
    """Add two integers."""
    return a + b


def test_python_tool_round_trip_and_streaming():
    agent = Agent("calc", provider=fake(call("add", a=2, b=3), say("The answer is 5", "5")), tools=[add])
    chunks = []
    r = agent.run("2+3?", on_delta=chunks.append)
    assert r.ok and r.output == "The answer is 5", r
    assert "".join(chunks) == "The answer is 5"
    assert r.run_id and r.trace_id and r.usage.input_tokens > 0


def test_schema_from_type_hints():
    assert add.parameters == {
        "type": "object",
        "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}},
        "required": ["a", "b"],
    }
    assert add.description == "Add two integers."
    assert add.read_only and add.name == "add"


def test_tool_exceptions_and_async_tools():
    @tool
    def fail() -> str:
        raise ValueError("disk full")

    @tool(name="later")
    async def later_(x: str) -> str:
        await asyncio.sleep(0.01)
        return f"later:{x}"

    agent = Agent(
        "t",
        provider=fake(call("fail"), call("later", x="ok"), say("done", "later:ok")),
        tools=[fail, later_],
    )
    r = agent.run("go")
    assert r.output == "done", r


def test_cancel_scope_from_another_thread():
    agent = Agent("slow", provider=fake(say("late", delayMs=20000)))
    scope = CancelScope()
    threading.Timer(0.3, scope.cancel).start()
    t = time.time()
    r = agent.run("x", cancel=scope)
    assert r.status == "cancelled" and time.time() - t < 5


def test_arun_and_task_cancellation():
    agent = Agent("a", provider=fake(say("hi"), say("slow", delayMs=20000)))

    async def main():
        r = await agent.arun("one")
        assert r.output == "hi"
        task = asyncio.ensure_future(agent.arun("two"))
        await asyncio.sleep(0.3)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(main())


def test_bundle_and_approvals():
    r = Agent(bundle=HELLO).run("hi")
    assert r.output == "Hello! Nice to meet you."

    asked = []

    @tool
    def pay(to: str) -> str:
        return f"paid {to}"

    agent = Agent(
        "payer",
        permissions={"default": "ask"},
        provider=fake(call("pay", to="bob"), say("ok", "paid bob")),
        tools=[pay],
        approve=lambda t, args: asked.append((t, args)) or True,
    )
    assert agent.run("pay bob").output == "ok"
    assert asked == [("pay", {"to": "bob"})]


def test_sessions_continue_conversation():
    agent = Agent("mem", provider=fake(say("one"), say("two", "second")))
    r1 = agent.run("first")
    r2 = agent.run("second", session_id=r1.session_id)
    assert r2.conversation_id == r1.conversation_id and r2.output == "two"


def test_errors_have_codes():
    with pytest.raises(AgenError) as e:
        Agent("x")  # no provider
    assert e.value.code == "invalid_spec"


def test_run_releases_the_gil():
    # A run holding the GIL would let the ticker in at most once. Counting
    # only ticks inside run(), with a generous margin, keeps this independent
    # of how coarse sleep() is on a busy CI machine.
    agent = Agent("slow", provider=fake(say("done", delayMs=1500)))
    running = threading.Event()
    stop = threading.Event()
    ticks = []

    def ticker():
        while not stop.is_set():
            if running.is_set():
                ticks.append(1)
            time.sleep(0.01)

    th = threading.Thread(target=ticker)
    th.start()
    running.set()
    agent.run("x")
    running.clear()
    stop.set()
    th.join()
    assert len(ticks) >= 5, f"other Python threads must keep running during run() ({len(ticks)} ticks)"


def test_closed_agent_and_tool_that_raises_base_exception():
    @tool
    def cancelled() -> str:
        raise asyncio.CancelledError()

    agent = Agent("t", provider=fake(call("cancelled"), say("ok", "CancelledError")), tools=[cancelled])
    assert agent.run("x").output == "ok"  # must not hang
    agent.close()
    with pytest.raises(AgenError) as e:
        agent.run("again")
    assert e.value.code == "closed"
    agent.close()  # idempotent


def test_cancel_reaches_a_running_tool():
    seen = threading.Event()

    @tool
    def wait(cancel: threading.Event) -> str:
        assert "cancel" not in wait_tool.parameters["properties"]
        cancel.wait(10)
        seen.set()
        return "stopped"

    wait_tool = wait
    agent = Agent("t", provider=fake(call("wait")), tools=[wait])
    scope = CancelScope()
    threading.Timer(0.3, scope.cancel).start()
    r = agent.run("x", cancel=scope)
    assert r.status == "cancelled"
    assert seen.wait(5), "tool never saw the cancel"
    assert agent._native.pending_requests() == 0


def test_early_cancel_scope():
    agent = Agent("t", provider=fake(say("never", delayMs=10000)))
    scope = CancelScope()
    scope.cancel()  # before the run starts
    assert agent.run("x", cancel=scope).status == "cancelled"


def test_agent_is_garbage_collected():
    import gc
    import weakref

    agent = Agent("t", provider=fake(say("hi")))
    ref = weakref.ref(agent)
    agent.run("x")
    del agent
    gc.collect()
    assert ref() is None, "Agent leaked (reference cycle through the native callback)"


def test_async_tools_run_on_the_callers_loop():
    loops = []

    @tool
    async def where() -> str:
        loops.append(asyncio.get_running_loop())
        return "here"

    agent = Agent("t", provider=fake(call("where"), say("done", "here")), tools=[where])

    async def main():
        r = await agent.arun("x")
        assert r.output == "done"
        assert loops == [asyncio.get_running_loop()]

    asyncio.run(main())


def test_non_json_tool_result_is_a_tool_error():
    @tool
    def weird() -> object:
        return object()

    agent = Agent("t", provider=fake(call("weird"), say("ok", "TypeError")), tools=[weird])
    assert agent.run("x").output == "ok"

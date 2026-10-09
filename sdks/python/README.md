# agen-sdk (Python)

Embed the Agen agent engine in a Python app. Same Rust engine as the Agen
platform, compiled into a native module.

```python
from agen import Agent, tool, openrouter

@tool(read_only=True)
def add(a: int, b: int) -> int:
    "Add two integers."
    return a + b

agent = Agent(
    "calc",
    instructions="Use the add tool for arithmetic, then answer with just the number.",
    model="deepseek/deepseek-v4-flash",
    provider=openrouter(),          # reads $OPENROUTER_API_KEY
    tools=[add],
)
result = agent.run("What is 1234 + 4321?", on_delta=lambda s: print(s, end=""))
print(result.output, result.usage)
```

- `Agent(bundle="path/to/bundle")` loads an agent bundle.
- `@tool` builds the JSON Schema from type hints; tools may be `async def`.
  Tools are side-effecting unless `read_only=True`; side-effecting calls go
  through the engine's effect ledger and are never silently repeated.
- `await agent.arun(...)` for asyncio; cancelling the task cancels the run.
- `CancelScope()` cancels a blocking `run` from another thread.
- `approve=lambda tool, args: ...` handles `ask` permissions (default: deny).
- `store="sqlite:agen.db"` persists sessions (default: in-memory).
- `run()` releases the GIL, so other threads keep running.

## Develop

```sh
python -m venv .venv && .venv/Scripts/pip install maturin pytest   # bin/ on Unix
.venv/Scripts/maturin develop
.venv/Scripts/python -m pytest tests
```

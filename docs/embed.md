# Embed an agent

The SDKs run the same engine as the fleet inside your own program: your code
starts the agent, gives it tools written in your language, and gets the
result back. No Hub or Nest is involved.

The SDKs are not on PyPI, npm or a Go module proxy yet. You build them from
the repository, which needs a [Rust toolchain](https://rustup.rs) (and, for
Go, a C compiler: MinGW-w64 GCC on Windows).

```sh
git clone https://github.com/prjvvl/agen && cd agen
```

## Python

```sh
python -m venv .venv
.venv/bin/pip install ./sdks/python      # Windows: .venv\Scripts\pip
```

```python
from agen import Agent, openrouter, tool

@tool(read_only=True)
def lookup_order(order_id: str) -> dict:
    """Look up an order by id."""
    return {"status": "shipped", "carrier": "UPS"}

agent = Agent("order-desk",
              instructions="Answer order questions using lookup_order.",
              model="deepseek/deepseek-v4-flash",
              provider=openrouter(),          # reads OPENROUTER_API_KEY
              tools=[lookup_order])

with agent:
    result = agent.run("Where is my order A-1001?")
    print(result.output)
```

Set `OPENROUTER_API_KEY` first. Without a key, run the sample, which uses a
scripted model: `.venv/bin/python examples/python-app/app.py`.

## Node.js

```sh
node sdks/node/scripts/build.js          # builds the native addon
node examples/node-app/app.js            # scripted model, no key
```

The [Node.js SDK README](https://github.com/prjvvl/agen/tree/main/sdks/node)
shows the API: `new Agent({...})`, `tool({...})`, `agent.run(...)`.

## Go

```sh
./scripts/build-ffi.sh                   # builds the static engine library
cd examples/go-app && go run .           # scripted model, no key
```

The [Go SDK README](https://github.com/prjvvl/agen/tree/main/sdks/go) shows the
API: `agen.New(agen.Spec{...}, agen.WithTool(...))`, `agent.Run(ctx, ...)`.

## What the SDKs share

- An agent from code (instructions, model, provider, tools) or from a bundle
  directory, the same format the fleet deploys.
- Tools in your language, side-effecting unless marked read-only, behind the
  same effect ledger as in a fleet.
- Streaming text, cancellation, and an approval callback for `ask`
  permissions (default: deny).
- One agent object is one conversation; a store (`sqlite:agen.db`) keeps it
  across restarts.

More in the [SDK reference](reference/sdks.md).

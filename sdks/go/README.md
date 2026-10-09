# Agen Go SDK

Embed the Agen agent engine in a Go program. The engine is the same Rust
engine the Agen platform runs; this package is a thin cgo wrapper over its C
ABI (`engine/crates/agen-ffi/include/agen.h`).

## Build

Requires Rust and a C compiler for cgo (MinGW-w64 GCC on Windows).

```sh
./scripts/build-ffi.sh          # builds the static engine library
cd sdks/go && go test ./...
```

## Use

```go
agent, err := agen.New(agen.Spec{
    Name:         "helper",
    Instructions: "Answer in one sentence. Use tools for arithmetic.",
    Model:        "deepseek/deepseek-v4-flash",
    Provider:     agen.OpenRouter(""), // reads $OPENROUTER_API_KEY
}, agen.WithTool(agen.Tool{
    Name:        "add",
    Description: "Add two integers a and b",
    Parameters:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
    ReadOnly:    true,
    Func: func(ctx context.Context, args json.RawMessage) (string, error) {
        var in struct{ A, B int }
        if err := json.Unmarshal(args, &in); err != nil {
            return "", err
        }
        return strconv.Itoa(in.A + in.B), nil
    },
}))
if err != nil { log.Fatal(err) }
defer agent.Close()

res, err := agent.Run(ctx, "What is 1234 + 4321?", agen.OnDelta(func(s string) { fmt.Print(s) }))
```

- `Spec{Bundle: "path/to/bundle"}` loads an agent bundle instead.
- Cancelling `ctx` cancels the run (`res.Status == "cancelled"`).
- Tools are side-effecting unless `ReadOnly`; side-effecting calls go through
  the engine's effect ledger and are never silently repeated.
- `agen.WithApprovals(fn)` handles `ask` permissions (default: denied).
- `Spec.Store` sets a persistent store (`sqlite:agen.db`, `postgres://…`);
  the default is in-memory.

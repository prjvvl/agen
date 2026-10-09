# Go sample: order desk

An embedded agent with a custom Go tool (`lookup_order`), streaming output and
the run's trace.

```sh
# from the repo root
./scripts/build-ffi.sh                     # builds the static engine library
cd examples/go-app
go run .                                   # scripted model, no key
go run . --live                            # needs $OPENROUTER_API_KEY
```

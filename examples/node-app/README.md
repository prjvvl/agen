# Node sample: order desk

An embedded agent with a custom JS tool (`lookup_order`), streaming output and
the run's trace.

```sh
# from the repo root
node sdks/node/scripts/build.js            # builds the native addon
node examples/node-app/app.js              # scripted model, no key
node examples/node-app/app.js --live       # needs $OPENROUTER_API_KEY
```

# Python sample: order desk

An embedded agent with a custom Python tool (`lookup_order`), streaming output
and the run's trace.

```sh
# from the repo root: build and install the SDK into a virtualenv
python -m venv .venv
.venv/bin/pip install ./sdks/python # Windows: .venv\Scripts; builds with maturin (needs Rust)

.venv/bin/python examples/python-app/app.py # scripted model, no key
.venv/bin/python examples/python-app/app.py --live # needs $OPENROUTER_API_KEY
```

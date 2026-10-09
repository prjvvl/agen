# Python sample: order desk

An embedded agent with a custom Python tool (`lookup_order`), streaming output
and the run's trace.

```sh
# from the repo root: build and install the SDK into a virtualenv
python -m venv .venv
.venv/Scripts/pip install ./sdks/python # .venv/bin on Linux/macOS; builds with maturin

.venv/Scripts/python examples/python-app/app.py # scripted model, no key
.venv/Scripts/python examples/python-app/app.py --live # needs $OPENROUTER_API_KEY
```

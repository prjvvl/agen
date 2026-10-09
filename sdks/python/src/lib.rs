//! `agen._native`: thin PyO3 bindings over `agen-sdk-core`. The friendly API
//! lives in the pure-Python package `agen`.

use std::sync::Arc;

use agen_sdk_core::{EventCallback, HostCallback, SdkAgent};
use pyo3::exceptions::PyRuntimeError;
use pyo3::prelude::*;

pyo3::create_exception!(
    _native,
    AgenError,
    PyRuntimeError,
    "Engine error; args[0] is a JSON {code, message}."
);

#[pyclass(frozen)]
struct NativeAgent {
    // None after close(): frees the engine agent (store, MCP servers).
    inner: std::sync::Mutex<Option<Arc<SdkAgent>>>,
}

impl NativeAgent {
    fn get(&self) -> PyResult<Arc<SdkAgent>> {
        self.inner
            .lock()
            .unwrap()
            .clone()
            .ok_or_else(|| AgenError::new_err(r#"{"code":"closed","message":"agent is closed"}"#))
    }
}

#[pymethods]
impl NativeAgent {
    /// Create from a JSON spec. `host(request_json)` is called (from engine
    /// threads) for host tools and approvals; answer with `complete`.
    #[new]
    #[pyo3(signature = (spec_json, host=None))]
    fn new(py: Python<'_>, spec_json: String, host: Option<Py<PyAny>>) -> PyResult<Self> {
        let host_cb: Option<HostCallback> = host.map(|h| {
            Arc::new(move |req: String| {
                Python::attach(|py| {
                    if let Err(e) = h.call1(py, (req,)) {
                        e.print(py);
                    }
                });
            }) as HostCallback
        });
        let agent = py
            .detach(|| SdkAgent::create(&spec_json, host_cb))
            .map_err(|e| AgenError::new_err(e.to_json()))?;
        Ok(Self {
            inner: std::sync::Mutex::new(Some(Arc::new(agent))),
        })
    }

    /// Run to completion, releasing the GIL. `on_event(event_json)` receives
    /// delta/reset/done events. Returns the result JSON.
    #[pyo3(signature = (input, options_json="", on_event=None))]
    fn run(&self, py: Python<'_>, input: String, options_json: &str, on_event: Option<Py<PyAny>>) -> PyResult<String> {
        let events: Option<EventCallback> = on_event.map(|cb| {
            Arc::new(move |ev: String| {
                Python::attach(|py| {
                    if let Err(e) = cb.call1(py, (ev,)) {
                        e.print(py);
                    }
                });
            }) as EventCallback
        });
        let agent = self.get()?;
        let options = options_json.to_string();
        py.detach(move || agent.run_blocking(&input, &options, events))
            .map_err(|e| AgenError::new_err(e.to_json()))
    }

    /// Answer a host request. Returns False if it is no longer pending.
    #[pyo3(signature = (request_id, result, is_error=false))]
    fn complete(&self, request_id: &str, result: String, is_error: bool) -> bool {
        match self.get() {
            Ok(a) => a.complete(request_id, if is_error { Err(result) } else { Ok(result) }),
            Err(_) => false,
        }
    }

    /// Cancel a run started with options {"cancelKey": key}.
    fn cancel(&self, key: &str) -> bool {
        self.get().map(|a| a.cancel(key)).unwrap_or(false)
    }

    fn tool_names(&self) -> PyResult<Vec<String>> {
        Ok(self.get()?.tool_names())
    }

    fn pending_requests(&self) -> usize {
        self.get().map(|a| a.pending_requests()).unwrap_or(0)
    }

    /// Spans of a trace as a JSON array.
    fn trace(&self, py: Python<'_>, trace_id: String) -> PyResult<String> {
        let agent = self.get()?;
        py.detach(move || agent.trace_blocking(&trace_id))
            .map_err(|e| AgenError::new_err(e.to_json()))
    }

    /// Close: reject new runs, cancel running ones and fail pending host
    /// requests. Idempotent. Resources are freed with this object.
    fn close(&self, py: Python<'_>) {
        if let Ok(agent) = self.get() {
            py.detach(move || agent.shutdown());
        }
    }
}

#[pymodule]
fn _native(m: &Bound<'_, PyModule>) -> PyResult<()> {
    m.add_class::<NativeAgent>()?;
    m.add("AgenError", m.py().get_type::<AgenError>())?;
    m.add("__version__", env!("CARGO_PKG_VERSION"))?;
    Ok(())
}

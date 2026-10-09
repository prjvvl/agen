//! Native Node.js bindings over `agen-sdk-core`. The friendly API lives in
//! `index.js`.
//!
//! - Runs return Promises and execute on the engine's own runtime, never on
//!   the JS thread.
//! - Host requests and run events reach JS through *weak* threadsafe
//!   functions, so an idle agent never keeps the process alive; a pending run
//!   Promise does.

use std::sync::{Arc, Mutex};

use agen_sdk_core::{EventCallback, HostCallback, RunSpec, SdkAgent};
use napi::bindgen_prelude::*;
use napi::threadsafe_function::{ThreadsafeFunction, ThreadsafeFunctionCallMode};
use napi_derive::napi;

type Tsfn = ThreadsafeFunction<String, (), String, Status, false, true>;

fn engine_error(e: agen_sdk_core::SdkError) -> Error {
    Error::new(Status::GenericFailure, e.to_json())
}

#[napi]
pub struct NativeAgent {
    inner: Mutex<Option<Arc<SdkAgent>>>,
}

#[napi]
impl NativeAgent {
    /// `host(requestJson)` receives tool/approval/cancel requests; answer
    /// with `complete`.
    #[napi(constructor)]
    pub fn new(spec_json: String, host: Option<Function<'_, String, ()>>) -> Result<Self> {
        // Set once the agent exists, so a request that cannot reach JS (queue
        // closing, env shutting down) can still be answered with an error.
        let me: Arc<std::sync::OnceLock<std::sync::Weak<SdkAgent>>> = Arc::default();
        let host_cb: Option<HostCallback> = match host {
            Some(f) => {
                let tsfn: Tsfn = f
                    .build_threadsafe_function::<String>()
                    .weak::<true>()
                    .callee_handled::<false>()
                    .build()?;
                let me = me.clone();
                Some(Arc::new(move |req: String| {
                    let status = tsfn.call(req.clone(), ThreadsafeFunctionCallMode::NonBlocking);
                    if status != Status::Ok {
                        fail_request(&me, &req);
                    }
                }) as HostCallback)
            }
            None => None,
        };
        let agent = Arc::new(SdkAgent::create(&spec_json, host_cb).map_err(engine_error)?);
        let _ = me.set(Arc::downgrade(&agent));
        Ok(Self {
            inner: Mutex::new(Some(agent)),
        })
    }

    fn get(&self) -> Result<Arc<SdkAgent>> {
        self.inner.lock().unwrap().clone().ok_or_else(|| {
            Error::new(
                Status::GenericFailure,
                r#"{"code":"closed","message":"agent is closed"}"#,
            )
        })
    }

    /// Run to completion. Resolves with the result JSON.
    #[napi(ts_return_type = "Promise<string>")]
    pub fn run<'a>(
        &self,
        env: &'a Env,
        input: String,
        options_json: String,
        on_event: Option<Function<'_, String, ()>>,
    ) -> Result<PromiseRaw<'a, String>> {
        let agent = self.get()?;
        let spec: RunSpec = if options_json.trim().is_empty() {
            RunSpec::default()
        } else {
            serde_json_parse(&options_json)?
        };
        let events: Option<EventCallback> = match on_event {
            Some(f) => {
                let tsfn: Tsfn = f
                    .build_threadsafe_function::<String>()
                    .weak::<true>()
                    .callee_handled::<false>()
                    .build()?;
                Some(Arc::new(move |ev: String| {
                    tsfn.call(ev, ThreadsafeFunctionCallMode::NonBlocking);
                }) as EventCallback)
            }
            None => None,
        };
        env.spawn_future(async move {
            let handle = agen_sdk_core::runtime().spawn(async move { agent.run_async(&input, spec, events).await });
            match handle.await {
                Ok(r) => r.map_err(engine_error),
                Err(e) => Err(Error::new(
                    Status::GenericFailure,
                    format!(r#"{{"code":"internal","message":"{e}"}}"#),
                )),
            }
        })
    }

    /// Spans of a trace as a JSON array.
    #[napi(ts_return_type = "Promise<string>")]
    pub fn trace<'a>(&self, env: &'a Env, trace_id: String) -> Result<PromiseRaw<'a, String>> {
        let agent = self.get()?;
        env.spawn_future(async move {
            let handle = agen_sdk_core::runtime().spawn(async move { agent.trace_async(&trace_id).await });
            match handle.await {
                Ok(r) => r.map_err(engine_error),
                Err(e) => Err(Error::new(
                    Status::GenericFailure,
                    format!(r#"{{"code":"internal","message":"{e}"}}"#),
                )),
            }
        })
    }

    /// Answer a host request. Returns false if it is no longer pending.
    #[napi]
    pub fn complete(&self, request_id: String, result: String, is_error: bool) -> bool {
        match self.get() {
            Ok(a) => a.complete(&request_id, if is_error { Err(result) } else { Ok(result) }),
            Err(_) => false,
        }
    }

    /// Cancel the run started with `cancelKey` (works before it starts, too).
    #[napi]
    pub fn cancel(&self, key: String) -> bool {
        self.get().map(|a| a.cancel(&key)).unwrap_or(false)
    }

    #[napi]
    pub fn pending_requests(&self) -> u32 {
        self.get().map(|a| a.pending_requests() as u32).unwrap_or(0)
    }

    #[napi]
    pub fn tool_names(&self) -> Result<Vec<String>> {
        Ok(self.get()?.tool_names())
    }

    /// Close: reject new runs, cancel running ones and fail pending host
    /// requests so run Promises settle. Idempotent.
    #[napi]
    pub fn close(&self) {
        if let Ok(agent) = self.get() {
            agent.shutdown();
        }
    }
}

/// Answer a tool/approval request that could not be delivered to JS.
fn fail_request(me: &std::sync::OnceLock<std::sync::Weak<SdkAgent>>, req: &str) {
    let Some(agent) = me.get().and_then(|w| w.upgrade()) else {
        return;
    };
    let Ok(v) = agen_sdk_core::parse_json(req) else { return };
    if v["kind"] != "cancel" {
        if let Some(id) = v["id"].as_str() {
            agent.complete(id, Err("JavaScript host unavailable".into()));
        }
    }
}

fn serde_json_parse(s: &str) -> Result<RunSpec> {
    agen_sdk_core::parse_run_spec(s).map_err(engine_error)
}

#[napi]
pub fn version() -> String {
    env!("CARGO_PKG_VERSION").to_string()
}

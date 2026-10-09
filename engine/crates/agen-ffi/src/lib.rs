//! C ABI over `agen-sdk-core` (used by the Go SDK; usable from any language
//! with a C FFI). All data crosses as UTF-8 JSON strings. Header:
//! `engine/crates/agen-ffi/include/agen.h`.
//!
//! Ownership: strings returned by this library must be freed with
//! `agen_string_free`. Callbacks may be invoked from any thread; they receive
//! a borrowed string valid only for the duration of the call.

use std::ffi::{c_char, c_void, CStr, CString};
use std::sync::Arc;

use agen_sdk_core::{EventCallback, HostCallback, SdkAgent};

/// Receives a JSON host request (tool call or approval). Answer later with
/// `agen_complete`.
pub type AgenHostCallback = Option<extern "C" fn(user_data: *mut c_void, request_json: *const c_char)>;
/// Receives a JSON run event.
pub type AgenEventCallback = Option<extern "C" fn(user_data: *mut c_void, event_json: *const c_char)>;

pub struct AgenAgent {
    inner: SdkAgent,
}

/// `user_data` pointers are owned by the caller and only passed back to it.
#[derive(Clone, Copy)]
struct UserData(*mut c_void);
// SAFETY: the pointer is never dereferenced here; the caller guarantees it is
// usable from any thread (the Go SDK passes a cgo.Handle value).
unsafe impl Send for UserData {}
unsafe impl Sync for UserData {}

fn to_cstring(s: String) -> *mut c_char {
    CString::new(s.replace('\0', ""))
        .map(CString::into_raw)
        .unwrap_or(std::ptr::null_mut())
}

unsafe fn str_arg<'a>(p: *const c_char) -> Result<&'a str, String> {
    if p.is_null() {
        return Ok("");
    }
    CStr::from_ptr(p)
        .to_str()
        .map_err(|e| format!("argument is not UTF-8: {e}"))
}

unsafe fn set_err(err_out: *mut *mut c_char, json: String) {
    if !err_out.is_null() {
        *err_out = to_cstring(json);
    }
}

fn call_back(cb: extern "C" fn(*mut c_void, *const c_char), ud: UserData, json: String) {
    if let Ok(c) = CString::new(json.replace('\0', "")) {
        cb(ud.0, c.as_ptr());
    }
}

/// Create an agent from a JSON spec. Returns NULL on error and sets
/// `*err_out` to a JSON error `{"code","message"}` (free it).
///
/// # Safety
/// `spec_json` must be a valid NUL-terminated string. `err_out` may be NULL.
#[no_mangle]
pub unsafe extern "C" fn agen_agent_new(
    spec_json: *const c_char,
    host_cb: AgenHostCallback,
    user_data: *mut c_void,
    err_out: *mut *mut c_char,
) -> *mut AgenAgent {
    let spec = match str_arg(spec_json) {
        Ok(s) => s,
        Err(e) => {
            set_err(
                err_out,
                serde_json::json!({"code": "invalid_argument", "message": e}).to_string(),
            );
            return std::ptr::null_mut();
        }
    };
    let ud = UserData(user_data);
    let host: Option<HostCallback> = host_cb.map(|cb| {
        Arc::new(move |req: String| {
            let ud = ud;
            call_back(cb, ud, req)
        }) as HostCallback
    });
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| SdkAgent::create(spec, host))) {
        Ok(Ok(a)) => Box::into_raw(Box::new(AgenAgent { inner: a })),
        Ok(Err(e)) => {
            set_err(err_out, e.to_json());
            std::ptr::null_mut()
        }
        Err(_) => {
            set_err(
                err_out,
                r#"{"code":"internal","message":"panic while creating agent"}"#.into(),
            );
            std::ptr::null_mut()
        }
    }
}

/// Run to completion (blocks the calling thread). Returns the result JSON, or
/// NULL with `*err_out` set. Events stream to `event_cb` while running.
///
/// # Safety
/// `agent` must come from `agen_agent_new` and not be freed during the call.
#[no_mangle]
pub unsafe extern "C" fn agen_run(
    agent: *const AgenAgent,
    input: *const c_char,
    options_json: *const c_char,
    event_cb: AgenEventCallback,
    user_data: *mut c_void,
    err_out: *mut *mut c_char,
) -> *mut c_char {
    let Some(agent) = agent.as_ref() else {
        set_err(
            err_out,
            r#"{"code":"invalid_argument","message":"agent is NULL"}"#.into(),
        );
        return std::ptr::null_mut();
    };
    let (input, options) = match (str_arg(input), str_arg(options_json)) {
        (Ok(i), Ok(o)) => (i, o),
        (Err(e), _) | (_, Err(e)) => {
            set_err(
                err_out,
                serde_json::json!({"code": "invalid_argument", "message": e}).to_string(),
            );
            return std::ptr::null_mut();
        }
    };
    let ud = UserData(user_data);
    let events: Option<EventCallback> = event_cb.map(|cb| {
        Arc::new(move |ev: String| {
            let ud = ud;
            call_back(cb, ud, ev)
        }) as EventCallback
    });
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        agent.inner.run_blocking(input, options, events)
    })) {
        Ok(Ok(result)) => to_cstring(result),
        Ok(Err(e)) => {
            set_err(err_out, e.to_json());
            std::ptr::null_mut()
        }
        Err(_) => {
            set_err(err_out, r#"{"code":"internal","message":"panic during run"}"#.into());
            std::ptr::null_mut()
        }
    }
}

/// Answer a host request. `is_error` != 0 marks `result` as an error message.
/// Returns 1 if the request was pending, 0 otherwise.
///
/// # Safety
/// `agent` must be valid; strings must be NUL-terminated.
#[no_mangle]
pub unsafe extern "C" fn agen_complete(
    agent: *const AgenAgent,
    request_id: *const c_char,
    result: *const c_char,
    is_error: i32,
) -> i32 {
    let Some(agent) = agent.as_ref() else { return 0 };
    let (Ok(id), Ok(res)) = (str_arg(request_id), str_arg(result)) else {
        return 0;
    };
    let r = if is_error != 0 {
        Err(res.to_string())
    } else {
        Ok(res.to_string())
    };
    agent.inner.complete(id, r) as i32
}

/// Cancel a run started with options `{"cancelKey": key}`. Returns 1 if found.
///
/// # Safety
/// `agent` must be valid; `key` NUL-terminated.
#[no_mangle]
pub unsafe extern "C" fn agen_cancel(agent: *const AgenAgent, key: *const c_char) -> i32 {
    let Some(agent) = agent.as_ref() else { return 0 };
    let Ok(k) = str_arg(key) else { return 0 };
    agent.inner.cancel(k) as i32
}

/// Spans of a trace as a JSON array, or NULL with `*err_out` set.
///
/// # Safety
/// `agent` must be valid; `trace_id` NUL-terminated.
#[no_mangle]
pub unsafe extern "C" fn agen_trace(
    agent: *const AgenAgent,
    trace_id: *const c_char,
    err_out: *mut *mut c_char,
) -> *mut c_char {
    let Some(agent) = agent.as_ref() else {
        set_err(
            err_out,
            r#"{"code":"invalid_argument","message":"agent is NULL"}"#.into(),
        );
        return std::ptr::null_mut();
    };
    let id = match str_arg(trace_id) {
        Ok(i) => i,
        Err(e) => {
            set_err(
                err_out,
                serde_json::json!({"code": "invalid_argument", "message": e}).to_string(),
            );
            return std::ptr::null_mut();
        }
    };
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| agent.inner.trace_blocking(id))) {
        Ok(Ok(s)) => to_cstring(s),
        Ok(Err(e)) => {
            set_err(err_out, e.to_json());
            std::ptr::null_mut()
        }
        Err(_) => {
            set_err(err_out, r#"{"code":"internal","message":"panic in trace"}"#.into());
            std::ptr::null_mut()
        }
    }
}

/// Close the agent: reject new runs, cancel running ones and fail pending host
/// requests so in-flight `agen_run` calls return. Call before `agen_agent_free`.
///
/// # Safety
/// `agent` must be valid.
#[no_mangle]
pub unsafe extern "C" fn agen_shutdown(agent: *const AgenAgent) {
    if let Some(agent) = agent.as_ref() {
        agent.inner.shutdown();
    }
}

/// Free an agent (closes its MCP servers and store).
///
/// # Safety
/// `agent` must come from `agen_agent_new` and not be used afterwards.
#[no_mangle]
pub unsafe extern "C" fn agen_agent_free(agent: *mut AgenAgent) {
    if !agent.is_null() {
        drop(Box::from_raw(agent));
    }
}

/// Free a string returned by this library.
///
/// # Safety
/// `s` must come from this library (or be NULL).
#[no_mangle]
pub unsafe extern "C" fn agen_string_free(s: *mut c_char) {
    if !s.is_null() {
        drop(CString::from_raw(s));
    }
}

/// Library version (static string, do not free).
#[no_mangle]
pub extern "C" fn agen_version() -> *const c_char {
    concat!(env!("CARGO_PKG_VERSION"), "\0").as_ptr() as *const c_char
}

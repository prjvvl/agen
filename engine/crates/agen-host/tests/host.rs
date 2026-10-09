//! agen-host as a real process: standalone run, managed-mode control API, and
//! crash safety when the process is killed in the middle of a side effect.

use std::io::BufRead;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use agen_engine::store::Store;
use serde_json::{json, Value};

const HOST: &str = env!("CARGO_BIN_EXE_agen-host");

fn hello() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../examples/bundles/hello")
}

fn store_url(dir: &Path) -> String {
    format!(
        "sqlite:{}",
        dir.join("agen.db").display().to_string().replace('\\', "/")
    )
}

fn write_bundle(dir: &Path, mcp: Option<Value>, script: Value) {
    std::fs::create_dir_all(dir.join("x-agen")).unwrap();
    std::fs::write(dir.join("plugin.json"), r#"{"name":"hosted"}"#).unwrap();
    std::fs::write(
        dir.join("x-agen/agent.md"),
        "---\nname: hosted\ndescription: host tests\n---\nDo the task.\n",
    )
    .unwrap();
    std::fs::write(
        dir.join("x-agen/harness.json"),
        r#"{"provider":"fake","model":"fake-1","script":"x-agen/script.json"}"#,
    )
    .unwrap();
    std::fs::write(dir.join("x-agen/script.json"), script.to_string()).unwrap();
    std::fs::write(dir.join("x-agen/config.json"), r#"{"permissions":{"default":"allow"}}"#).unwrap();
    if let Some(m) = mcp {
        std::fs::write(dir.join("mcp.json"), m.to_string()).unwrap();
    }
}

#[test]
fn run_prints_json_result() {
    let dir = tempfile::tempdir().unwrap();
    let out = Command::new(HOST)
        .args([
            "run",
            hello().to_str().unwrap(),
            "--input",
            "hi",
            "--json",
            "--store",
            &store_url(dir.path()),
        ])
        .output()
        .unwrap();
    assert!(out.status.success(), "{}", String::from_utf8_lossy(&out.stderr));
    let v: Value = serde_json::from_slice(&out.stdout).unwrap();
    assert_eq!(v["status"], "succeeded");
    assert_eq!(v["output"], "Hello! Nice to meet you.");
}

#[test]
fn invalid_bundle_fails_with_clear_error() {
    let dir = tempfile::tempdir().unwrap();
    let out = Command::new(HOST)
        .args(["run", dir.path().to_str().unwrap(), "--input", "x"])
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(2));
    let err = String::from_utf8_lossy(&out.stderr);
    assert!(
        err.contains("plugin.json") && err.contains("required file is missing"),
        "{err}"
    );
}

struct Served {
    child: Child,
    base: String,
}

impl Drop for Served {
    fn drop(&mut self) {
        let _ = self.child.kill();
    }
}

fn serve(bundle: &Path, store: &str, extra: &[&str]) -> Served {
    let mut child = Command::new(HOST)
        .args([
            "serve",
            bundle.to_str().unwrap(),
            "--store",
            store,
            "--instance-id",
            "inst-1",
        ])
        .args(extra)
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    let mut line = String::new();
    std::io::BufReader::new(child.stdout.take().unwrap())
        .read_line(&mut line)
        .unwrap();
    let base = line
        .trim()
        .strip_prefix("listening ")
        .expect("listening line")
        .to_string();
    Served { child, base }
}

async fn call(base: &str, method: &str, body: Value) -> (u16, Value) {
    let r = reqwest::Client::new()
        .post(format!("{base}/agen.v1.HostService/{method}"))
        .json(&body)
        .send()
        .await
        .unwrap();
    (r.status().as_u16(), r.json().await.unwrap_or(Value::Null))
}

#[tokio::test]
async fn serve_mode_control_api() {
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path().join("b");
    write_bundle(
        &b,
        None,
        json!({"responses": [{"text": "slow answer", "delayMs": 3000}, {"text": "quick"}], "cycle": true}),
    );
    let s = serve(&b, &store_url(dir.path()), &["--max-concurrency", "1"]);

    let (st, h) = call(&s.base, "Health", json!({})).await;
    assert_eq!(
        (st, h["ready"].clone(), h["instanceId"].clone()),
        (200, json!(true), json!("inst-1"))
    );

    // A long task, then a second one hits the concurrency limit, then cancel.
    let base = s.base.clone();
    let long = tokio::spawn(async move { call(&base, "RunTask", json!({"task": {"id": "t1", "input": "go"}})).await });
    tokio::time::sleep(Duration::from_millis(500)).await;
    let (st, e) = call(&s.base, "RunTask", json!({"task": {"id": "t2", "input": "go"}})).await;
    assert_eq!((st, e["code"].clone()), (429, json!("resource_exhausted")));
    let (_, h) = call(&s.base, "Health", json!({})).await;
    assert_eq!(h["runningTasks"], 1);
    let (_, c) = call(&s.base, "CancelTask", json!({"taskId": "t1"})).await;
    assert_eq!(c["cancelled"], true);
    let (st, r) = long.await.unwrap();
    assert_eq!(st, 200);
    assert_eq!(
        (r["success"].clone(), r["run"]["status"].clone()),
        (json!(false), json!("cancelled"))
    );

    let (st, r) = call(&s.base, "RunTask", json!({"task": {"id": "t3", "input": "again"}})).await;
    assert_eq!(st, 200, "{r}");
    assert_eq!(r["output"], "quick");

    let (st, _) = call(&s.base, "Drain", json!({})).await;
    assert_eq!(st, 200);
    let (st, e) = call(&s.base, "RunTask", json!({"task": {"id": "t4", "input": "x"}})).await;
    assert_eq!((st, e["code"].clone()), (503, json!("unavailable")));
}

fn process_alive(pid: u32) -> bool {
    if cfg!(windows) {
        let out = Command::new("tasklist")
            .args(["/FI", &format!("PID eq {pid}"), "/NH"])
            .output()
            .unwrap();
        String::from_utf8_lossy(&out.stdout).contains(&pid.to_string())
    } else {
        Command::new("kill")
            .args(["-0", &pid.to_string()])
            .status()
            .map(|s| s.success())
            .unwrap_or(false)
    }
}

/// Kill the host process mid side effect, resume in a new process, and
/// prove the side effect is not repeated.
#[tokio::test]
async fn killed_mid_side_effect_resumes_without_repeating_it() {
    let server = agen_testkit::bin_path("agen-testkit", "mcp-test-server");
    let dir = tempfile::tempdir().unwrap();
    let notes = dir.path().join("notes.txt");
    let pid_file = dir.path().join("mcp.pid");
    let b = dir.path().join("b");
    let mcp = json!({"mcpServers": {"bank": {
        "command": server.display().to_string(),
        "env": {"NOTES_FILE": notes.display().to_string(), "PID_FILE": pid_file.display().to_string()}
    }}});
    write_bundle(
        &b,
        Some(mcp.clone()),
        json!({"responses": [
            {"toolCalls": [{"name": "bank.write_note_slow", "arguments": {"text": "pay bob 100", "ms": 60000}}]}
        ]}),
    );
    let store = store_url(dir.path());
    let mut host = Command::new(HOST)
        .args([
            "run",
            b.to_str().unwrap(),
            "--input",
            "pay bob",
            "--json",
            "--store",
            &store,
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    // Wait until the payment has been made (the tool is now stalled).
    let deadline = Instant::now() + Duration::from_secs(60);
    while !std::fs::read_to_string(&notes)
        .unwrap_or_default()
        .contains("pay bob 100")
    {
        assert!(Instant::now() < deadline, "payment never happened");
        assert!(host.try_wait().unwrap().is_none(), "host exited early");
        std::thread::sleep(Duration::from_millis(50));
    }
    let mcp_pid: u32 = std::fs::read_to_string(&pid_file).unwrap().trim().parse().unwrap();
    host.kill().unwrap(); // hard kill: TerminateProcess / SIGKILL
    host.wait().unwrap();

    // The MCP server child must not outlive the killed host.
    let deadline = Instant::now() + Duration::from_secs(10);
    while process_alive(mcp_pid) {
        assert!(
            Instant::now() < deadline,
            "mcp server {mcp_pid} survived the host being killed"
        );
        std::thread::sleep(Duration::from_millis(100));
    }

    let s = Store::open(&store).await.unwrap();
    let unfinished = s.unfinished_runs("default", "hosted").await.unwrap();
    assert_eq!(unfinished.len(), 1, "the killed run is unfinished");
    let run_id = unfinished[0].id.clone();
    s.close().await;

    // A new process resumes. The model is told the outcome is unknown.
    write_bundle(
        &b,
        Some(mcp),
        json!({"responses": [
            {"expect": "effect_unknown", "text": "Payment status unknown; not retrying."}
        ]}),
    );
    let out = Command::new(HOST)
        .args(["resume", &run_id, b.to_str().unwrap(), "--json", "--store", &store])
        .output()
        .unwrap();
    assert!(
        out.status.success(),
        "resume failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    let v: Value = serde_json::from_slice(&out.stdout).unwrap();
    assert_eq!(v["status"], "succeeded");
    assert_eq!(v["id"], run_id.as_str());
    assert_eq!(
        std::fs::read_to_string(&notes).unwrap(),
        "pay bob 100\n",
        "payment must happen exactly once"
    );
}

/// A task re-sent to a new host after its first host died resumes the same
/// run instead of starting a second one (Manager re-lease semantics).
#[tokio::test]
async fn resent_task_resumes_its_run_on_a_new_host() {
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path().join("b");
    let store = store_url(dir.path());
    write_bundle(
        &b,
        None,
        json!({"responses": [{"text": "never finishes", "delayMs": 60000}]}),
    );
    let s1 = serve(&b, &store, &["--singleton"]);
    let base = s1.base.clone();
    let _pending =
        tokio::spawn(async move { call(&base, "RunTask", json!({"task": {"id": "task-A", "input": "work"}})).await });
    let st = Store::open(&store).await.unwrap();
    let deadline = Instant::now() + Duration::from_secs(20);
    let first_run = loop {
        if let Some(r) = st.run_by_task("default", "hosted", "task-A").await.unwrap() {
            break r;
        }
        assert!(Instant::now() < deadline, "task never started");
        tokio::time::sleep(Duration::from_millis(50)).await;
    };
    drop(s1); // the host dies

    write_bundle(&b, None, json!({"responses": [{"text": "finished by host 2"}]}));
    let s2 = serve(&b, &store, &["--singleton"]);
    let (code, r) = call(&s2.base, "RunTask", json!({"task": {"id": "task-A", "input": "work"}})).await;
    assert_eq!(code, 200, "{r}");
    assert_eq!(r["run"]["id"], first_run.id.as_str(), "must resume the original run");
    assert_eq!(r["output"], "finished by host 2");
    // And a later task in the singleton conversation is not blocked.
    let (code, r) = call(&s2.base, "RunTask", json!({"task": {"id": "task-B", "input": "next"}})).await;
    assert_eq!(code, 200, "{r}");
}

/// A caller that disconnects mid-task must not leak a concurrency slot.
#[tokio::test]
async fn client_disconnect_does_not_leak_a_slot() {
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path().join("b");
    write_bundle(
        &b,
        None,
        json!({"responses": [{"text": "done", "delayMs": 1500}], "cycle": true}),
    );
    let s = serve(&b, &store_url(dir.path()), &["--max-concurrency", "1"]);
    let short = reqwest::Client::builder()
        .timeout(Duration::from_millis(200))
        .build()
        .unwrap();
    let r = short
        .post(format!("{}/agen.v1.HostService/RunTask", s.base))
        .json(&json!({"task": {"id": "gone", "input": "x"}}))
        .send()
        .await;
    assert!(r.is_err(), "client should have timed out");
    tokio::time::sleep(Duration::from_millis(2500)).await;
    let (_, h) = call(&s.base, "Health", json!({})).await;
    assert_eq!(h["runningTasks"], 0, "{h}");
    let (code, r) = call(&s.base, "RunTask", json!({"task": {"id": "next", "input": "x"}})).await;
    assert_eq!(code, 200, "{r}");
}

#[tokio::test]
async fn bad_json_is_invalid_argument() {
    let dir = tempfile::tempdir().unwrap();
    let s = serve(&hello(), &store_url(dir.path()), &[]);
    let r = reqwest::Client::new()
        .post(format!("{}/agen.v1.HostService/RunTask", s.base))
        .header("content-type", "application/json")
        .body("{not json")
        .send()
        .await
        .unwrap();
    assert_eq!(r.status().as_u16(), 400);
    let v: Value = r.json().await.unwrap();
    assert_eq!(v["code"], "invalid_argument");
}

/// With AGEN_HOST_TOKEN set, HostService answers only callers presenting it.
#[tokio::test]
async fn host_token_is_required_when_set() {
    let dir = tempfile::tempdir().unwrap();
    let mut child = Command::new(HOST)
        .args(["serve", hello().to_str().unwrap(), "--store", &store_url(dir.path())])
        .env("AGEN_HOST_TOKEN", "host-secret-123")
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    let mut line = String::new();
    std::io::BufReader::new(child.stdout.take().unwrap())
        .read_line(&mut line)
        .unwrap();
    let s = Served {
        base: line.trim().strip_prefix("listening ").unwrap().to_string(),
        child,
    };
    let health = |auth: Option<&'static str>| {
        let base = s.base.clone();
        async move {
            let mut req = reqwest::Client::new()
                .post(format!("{base}/agen.v1.HostService/Health"))
                .json(&json!({}));
            if let Some(a) = auth {
                req = req.header("authorization", a);
            }
            let r = req.send().await.unwrap();
            let status = r.status().as_u16();
            (status, r.json::<Value>().await.unwrap_or(Value::Null))
        }
    };
    let (st, v) = health(None).await;
    assert_eq!((st, v["code"].clone()), (401, json!("unauthenticated")));
    assert_eq!(health(Some("Bearer wrong-token-xyz")).await.0, 401);
    let (st, v) = health(Some("Bearer host-secret-123")).await;
    assert_eq!((st, v["ready"].clone()), (200, json!(true)));
}

/// At the process boundary: a secret the model echoes never appears on the
/// host's stdout or stderr, in `run` (streamed) or `serve` (managed) mode.
#[tokio::test]
async fn host_output_never_contains_secret_values() {
    let dir = tempfile::tempdir().unwrap();
    let b = dir.path().join("b");
    let secret = "sk-host-secret-4242";
    write_bundle(
        &b,
        None,
        json!({"cycle": true, "responses": [{"text": format!("the key is {secret} ok")}]}),
    );
    std::fs::write(b.join("x-agen/secrets.json"), r#"{"DEMO_KEY":{"source":"env"}}"#).unwrap();
    // Streamed run.
    let out = Command::new(HOST)
        .args([
            "run",
            b.to_str().unwrap(),
            "--input",
            "tell me",
            "--store",
            &store_url(dir.path()),
        ])
        .env("DEMO_KEY", secret)
        .output()
        .unwrap();
    let (so, se) = (
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr),
    );
    assert!(out.status.success(), "{se}");
    assert!(
        so.contains("the key is") && !so.contains(secret) && !se.contains(secret),
        "stdout: {so}\nstderr: {se}"
    );
    // Managed mode: RunTask result and process output.
    let mut child = Command::new(HOST)
        .args(["serve", b.to_str().unwrap(), "--store", &store_url(dir.path())])
        .env("DEMO_KEY", secret)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let mut stdout = std::io::BufReader::new(child.stdout.take().unwrap());
    let mut line = String::new();
    stdout.read_line(&mut line).unwrap();
    let url = line.trim().strip_prefix("listening ").unwrap().to_string();
    let resp = reqwest::Client::new()
        .post(format!("{url}/agen.v1.HostService/RunTask"))
        .json(&json!({"task": {"id": "t-secret", "input": "tell me"}}))
        .send()
        .await
        .unwrap()
        .text()
        .await
        .unwrap();
    child.kill().unwrap();
    let mut rest = String::new();
    std::io::Read::read_to_string(&mut stdout, &mut rest).unwrap();
    let mut err = String::new();
    std::io::Read::read_to_string(&mut child.stderr.take().unwrap(), &mut err).unwrap();
    let _ = child.wait();
    assert!(resp.contains("the key is") && !resp.contains(secret), "{resp}");
    assert!(!rest.contains(secret) && !err.contains(secret) && !line.contains(secret));
}

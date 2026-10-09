import { useState } from "react";
import { call, Deployment, LogLine, lower, Run, short, Task, TriggerEvent } from "../api";
import { errorText, usePoll } from "../hooks";
import { status } from "./Deployments";

const time = (t?: string) => (t ? new Date(t).toLocaleTimeString() : "");

export function DeploymentDetail({ namespace, name }: { namespace: string; name: string }) {
  const ref = { namespace, name };
  const dep = usePoll(() => call<{ deployment: Deployment }>("GetDeployment", { ref }), [namespace, name]);
  const tasks = usePoll(() => call<{ tasks?: Task[] }>("ListTasks", { namespace, deployment: name, limit: 20 }), [namespace, name]);
  const runs = usePoll(() => call<{ runs?: Run[] }>("ListRuns", { namespace, deployment: name, limit: 20 }), [namespace, name]);
  const events = usePoll(() => call<{ events?: TriggerEvent[] }>("ListTriggerEvents", { ref, limit: 20 }), [namespace, name], 10000);
  const logs = usePoll(() => call<{ lines?: LogLine[] }>("GetLogs", { ref, limit: 50 }), [namespace, name], 5000);
  const [input, setInput] = useState("");
  const [msg, setMsg] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      const r = await call<{ task: Task }>("SubmitTask", { ref, input });
      setMsg(`Task ${r.task.id} queued`);
      setInput("");
      tasks.reload();
    } catch (err) {
      setMsg(errorText(err));
    }
  }
  async function remove() {
    try {
      await call("DeleteDeployment", { ref });
      location.hash = "#/deployments";
    } catch (err) {
      setMsg(errorText(err));
    }
  }

  const d = dep.data?.deployment;
  if (dep.error) return <p className="error">{dep.error}</p>;
  if (!d) return <p>Loading…</p>;
  return (
    <section>
      <h2>
        {d.namespace}/{d.name} <span className="badge">{lower(d.kind, "DEPLOYMENT_KIND_")}</span>
      </h2>
      <p className="facts">
        <span data-field="status">{status(d)}</span> · ready {d.ready ?? 0} / desired {d.desired ?? 0} · scale {d.scale?.min ?? 0}..
        {d.scale?.max ?? 1} · definition <span className="mono">{short(d.definitionDigest)}</span>
        {d.budget?.maxUsdPerDay ? ` · spent today $${(d.spentUsdToday ?? 0).toFixed(4)} of $${d.budget.maxUsdPerDay}` : ""}
      </p>
      <p>
        <a href={`#/edit/${namespace}/${name}`}>Edit bundle</a>{" "}
        {confirmDelete ? (
          <>
            <button className="danger small" onClick={remove}>
              Confirm delete
            </button>{" "}
            <button className="small" onClick={() => setConfirmDelete(false)}>
              Keep
            </button>
          </>
        ) : (
          <button className="small" onClick={() => setConfirmDelete(true)}>
            Delete…
          </button>
        )}
      </p>

      <form className="row" onSubmit={submit}>
        <input value={input} onChange={(e) => setInput(e.target.value)} placeholder="Task input" aria-label="Task input" />
        <button type="submit" disabled={!input}>
          Submit task
        </button>
      </form>
      {msg && <p className="note">{msg}</p>}

      <h3>Tasks</h3>
      <table>
        <thead>
          <tr>
            <th>Task</th>
            <th>State</th>
            <th>Source</th>
            <th>Output</th>
            <th>Created</th>
          </tr>
        </thead>
        <tbody>
          {(tasks.data?.tasks ?? []).map((t) => (
            <tr key={t.id} data-task={t.id}>
              <td className="mono">{short(t.id, 26)}</td>
              <td data-field="state">{lower(t.state, "TASK_STATE_")}</td>
              <td>{t.source}</td>
              <td>{t.output || t.error}</td>
              <td>{time(t.createdAt)}</td>
            </tr>
          ))}
        </tbody>
      </table>

      <h3>Runs</h3>
      <table>
        <thead>
          <tr>
            <th>Run</th>
            <th>Status</th>
            <th>Tokens</th>
            <th>Cost</th>
            <th>Trace</th>
          </tr>
        </thead>
        <tbody>
          {(runs.data?.runs ?? []).map((r) => (
            <tr key={r.id}>
              <td className="mono">{short(r.id, 26)}</td>
              <td>{r.status}</td>
              <td>
                {r.usage?.inputTokens ?? 0}+{r.usage?.outputTokens ?? 0}
              </td>
              <td>{r.usage?.costUsd ? `$${r.usage.costUsd.toFixed(4)}` : ""}</td>
              <td>{r.traceId && <a href={`#/trace/${r.traceId}`}>trace</a>}</td>
            </tr>
          ))}
        </tbody>
      </table>

      {(events.data?.events ?? []).length > 0 && (
        <>
          <h3>Trigger events</h3>
          <table>
            <tbody>
              {events.data!.events!.map((e, i) => (
                <tr key={i}>
                  <td>{e.trigger}</td>
                  <td>{lower(e.state, "TRIGGER_EVENT_STATE_")}</td>
                  <td>{time(e.dueAt)}</td>
                  <td>{e.message}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      <h3>Logs</h3>
      <pre className="logs">
        {(logs.data?.lines ?? []).map((l) => `${time(l.time)} ${short(l.instanceId, 8)} ${l.level.padEnd(5)} ${l.message}`).join("\n") || "No log lines."}
      </pre>
    </section>
  );
}

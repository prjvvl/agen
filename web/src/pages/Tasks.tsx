import { ExternalLink } from "lucide-react";
import { call, Task } from "../api";
import { useQuery } from "../live";
import { deploymentHref, traceHref } from "../router";
import { Alert, Badge, Copy, Empty, Panel, SkeletonRows, Status, taskStatus, Time } from "../ui";

export function TasksTable({
  namespace,
  deployment,
  limit = 50,
  onOpen,
}: {
  namespace?: string;
  deployment?: string;
  limit?: number;
  onOpen: (id: string) => void;
}) {
  const tasks = useQuery(() => call<{ tasks?: Task[] }>("ListTasks", { namespace, deployment, limit }), [namespace, deployment, limit], {
    on: (e) => (e.kind === "task" || e.kind === "approval") && (!deployment || e.deployment === deployment),
  });
  if (tasks.error) return <Alert>{tasks.error}</Alert>;
  if (tasks.loading) return <SkeletonRows rows={Math.min(limit, 4)} cols={4} />;
  const list = tasks.data?.tasks ?? [];
  if (!list.length) return <Empty title="No tasks yet">Submit one with Run a task, the API or a trigger.</Empty>;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Task</th>
            <th>State</th>
            <th>Source</th>
            <th>Result</th>
            <th>Created</th>
          </tr>
        </thead>
        <tbody>
          {list.map((t) => (
            <tr key={t.id} data-task={t.id} className="clickable" onClick={() => onOpen(t.id)}>
              <td>
                <div className="cell-main">
                  <button className="btn ghost sm" style={{ padding: 0, height: "auto" }} onClick={() => onOpen(t.id)}>
                    <span className="truncate" style={{ maxWidth: 320 }}>
                      {t.input || <span className="muted">(empty input)</span>}
                    </span>
                  </button>
                  {!deployment && <span className="sub">{t.deployment}</span>}
                </div>
              </td>
              <td data-field="state">
                <Status spec={taskStatus(t.state, t.pendingApprovalId)} />
              </td>
              <td className="muted nowrap">{t.source}</td>
              <td>
                <span className="truncate" style={{ display: "block", maxWidth: 380, color: t.error ? "var(--err)" : undefined }} title={t.output || t.error}>
                  {t.output || t.error}
                </span>
              </td>
              <td>
                <Time t={t.createdAt} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function TaskPanel({ id, onClose }: { id: string; onClose: () => void }) {
  const task = useQuery(() => call<{ task: Task }>("GetTask", { id }), [id], {
    on: (e) => (e.kind === "task" && e.id === id) || e.kind === "approval",
  });
  const t = task.data?.task;
  return (
    <Panel title="Task" onClose={onClose}>
      {task.error && <Alert>{task.error}</Alert>}
      {!t ? (
        <SkeletonRows rows={3} cols={2} />
      ) : (
        <>
          <div className="toolbar">
            <Status spec={taskStatus(t.state, t.pendingApprovalId)} />
            <span className="grow" />
            {t.traceId && (
              <a className="btn sm" href={traceHref(t.traceId)}>
                <ExternalLink size={13} aria-hidden />
                Open trace
              </a>
            )}
          </div>
          {t.pendingApprovalId && (
            <Alert tone="warn">
              The run waits for an approval. <a href="#/inbox">Decide it in the inbox</a>.
            </Alert>
          )}
          <section className="section">
            <h3>Input</h3>
            <pre className="code">{t.input || "(empty)"}</pre>
          </section>
          {(t.output || t.error) && (
            <section className="section">
              <h3>{t.error ? "Error" : "Output"}</h3>
              <pre className="code" style={t.error ? { color: "var(--err)" } : undefined}>
                {t.output || t.error}
              </pre>
            </section>
          )}
          <dl className="kv">
            <dt>Task</dt>
            <dd>
              <Copy text={t.id} />
            </dd>
            <dt>Deployment</dt>
            <dd>
              <a href={deploymentHref(t.namespace, t.deployment)}>
                {t.namespace}/{t.deployment}
              </a>
            </dd>
            <dt>Source</dt>
            <dd>{t.source}</dd>
            {t.runId && (
              <>
                <dt>Run</dt>
                <dd>
                  <Copy text={t.runId} />
                </dd>
              </>
            )}
            {(t.attempts ?? 0) > 1 && (
              <>
                <dt>Attempts</dt>
                <dd>{t.attempts}</dd>
              </>
            )}
            {t.conversationKey && (
              <>
                <dt>Conversation</dt>
                <dd>
                  <Copy text={t.conversationKey} />
                </dd>
              </>
            )}
            {t.labels && Object.keys(t.labels).length > 0 && (
              <>
                <dt>Labels</dt>
                <dd className="toolbar">
                  {Object.entries(t.labels).map(([k, v]) => (
                    <Badge key={k}>
                      {k}={v}
                    </Badge>
                  ))}
                </dd>
              </>
            )}
            <dt>Created</dt>
            <dd>
              <Time t={t.createdAt} />
            </dd>
          </dl>
        </>
      )}
    </Panel>
  );
}

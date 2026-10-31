import { ExternalLink } from "lucide-react";
import { useEffect, useState } from "react";
import { call, Task } from "../api";
import { useQuery } from "../live";
import { deploymentHref, traceHref } from "../router";
import { Alert, Badge, Copy, Empty, outcomeStatus, Panel, SkeletonRows, Status, taskStatus, Time } from "../ui";

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
                <div className="cell-main">
                  <Status spec={taskStatus(t.state, t.pendingApprovalId, t.notBefore, t.waitReason)} />
                  <OutcomeStatus task={t} />
                </div>
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

function OutcomeStatus({ task }: { task: Task }) {
  const spec = outcomeStatus(task.outcome);
  return spec ? (
    <span data-field="outcome">
      <Status spec={spec} />
    </span>
  ) : null;
}

export function TaskPanel({ id, onClose }: { id: string; onClose: () => void }) {
  return (
    <Panel title="Task" onClose={onClose}>
      <TaskDetails id={id} />
    </Panel>
  );
}

/**
 * A task's state, input, result and identity, kept live. Clicking another
 * task in its Tree section re-points this same panel at that task,
 * instead of opening a second one.
 */
export function TaskDetails({ id }: { id: string }) {
  const [viewId, setViewId] = useState(id);
  useEffect(() => setViewId(id), [id]);
  const task = useQuery(() => call<{ task: Task }>("GetTask", { id: viewId }), [viewId], {
    on: (e) => (e.kind === "task" && e.id === viewId) || e.kind === "approval",
  });
  const t = task.data?.task;
  return (
    <>
      {task.error && <Alert>{task.error}</Alert>}
      {!t ? (
        <SkeletonRows rows={3} cols={2} />
      ) : (
        <>
          <div className="toolbar">
            <Status spec={taskStatus(t.state, t.pendingApprovalId, t.notBefore, t.waitReason)} />
            <OutcomeStatus task={t} />
            <span className="grow" />
            {!["TASK_STATE_SUCCEEDED", "TASK_STATE_FAILED", "TASK_STATE_CANCELLED"].includes(t.state) && (
              <button className="btn sm" onClick={() => call("CancelTask", { id: t.id }).then(() => task.reload())}>
                Cancel task
              </button>
            )}
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
              <h3>{t.error ? "Error" : t.outcome && t.outcome.status !== "done" ? "Report" : "Output"}</h3>
              <pre className="code" style={t.error ? { color: "var(--err)" } : undefined}>
                {t.output || t.error}
              </pre>
            </section>
          )}
          {t.outcome?.data !== undefined && (
            <section className="section" data-field="outcome-data">
              <h3>Result data</h3>
              <pre className="code">{JSON.stringify(t.outcome.data, null, 2)}</pre>
            </section>
          )}
          {t.rootTaskId && <TaskTree rootTaskId={t.rootTaskId} currentId={viewId} onOpen={setViewId} />}
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
            {t.notBefore && (
              <>
                <dt>Starts</dt>
                <dd>
                  {new Date(t.notBefore).toLocaleString()} <span className="muted">(<Time t={t.notBefore} />)</span>
                </dd>
              </>
            )}
            {(t.modelTier || t.budgetPressure) && (
              <>
                <dt>Model tier</dt>
                <dd>
                  {t.modelTier || "default"}
                  {t.budgetPressure && <span className="muted"> · near the daily budget, so the cheapest tier</span>}
                </dd>
              </>
            )}
            {t.replyDeployment && (
              <>
                <dt>Result goes to</dt>
                <dd>
                  <a href={deploymentHref(t.namespace, t.replyDeployment)}>{t.replyDeployment}</a>
                  {t.replyConversationKey && <span className="muted"> · conversation {t.replyConversationKey}</span>}
                </dd>
              </>
            )}
            {t.parentTaskId && (
              <>
                <dt>Started by task</dt>
                <dd>
                  <Copy text={t.parentTaskId} />
                </dd>
              </>
            )}
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
    </>
  );
}

interface TreeNode {
  task: Task;
  children: TreeNode[];
}

function buildTaskTree(tasks: Task[]): TreeNode[] {
  const nodes = new Map<string, TreeNode>();
  for (const t of tasks) nodes.set(t.id, { task: t, children: [] });
  const roots: TreeNode[] = [];
  for (const n of nodes.values()) {
    const p = n.task.parentTaskId ? nodes.get(n.task.parentTaskId) : undefined;
    if (p) p.children.push(n);
    else roots.push(n);
  }
  const bySubmitted = (a: TreeNode, b: TreeNode) => (a.task.createdAt ?? "").localeCompare(b.task.createdAt ?? "");
  const fix = (n: TreeNode) => {
    n.children.sort(bySubmitted);
    n.children.forEach(fix);
  };
  roots.sort(bySubmitted).forEach(fix);
  return roots;
}

/** Every task of the current one's tree, oldest first (`GetTaskTree`); a
 * row other than the one already shown opens it in this same panel. */
function TaskTree({ rootTaskId, currentId, onOpen }: { rootTaskId: string; currentId: string; onOpen: (id: string) => void }) {
  const tree = useQuery(() => call<{ tasks?: Task[] }>("GetTaskTree", { taskId: rootTaskId }), [rootTaskId], {
    on: (e) => e.kind === "task",
  });
  const list = tree.data?.tasks ?? [];
  // A lone task is its own whole tree: nothing to show. A load error here
  // is quiet (no Alert) so the rest of the panel still works without it.
  if (tree.error || list.length < 2) return null;
  const roots = buildTaskTree(list);
  return (
    <section className="section">
      <h3>Tree</h3>
      <div role="tree" aria-label="Task tree" className="task-tree">
        {roots.map((r) => (
          <TaskTreeRow key={r.task.id} node={r} depth={0} currentId={currentId} onOpen={onOpen} />
        ))}
      </div>
    </section>
  );
}

function TaskTreeRow({ node, depth, currentId, onOpen }: { node: TreeNode; depth: number; currentId: string; onOpen: (id: string) => void }) {
  const t = node.task;
  const isCurrent = t.id === currentId;
  return (
    <>
      <div
        role="treeitem"
        aria-selected={isCurrent}
        aria-level={depth + 1}
        className="task-tree-row"
        aria-current={isCurrent || undefined}
        style={{ paddingLeft: 8 + depth * 16 }}
        onClick={() => !isCurrent && onOpen(t.id)}
      >
        <Status spec={taskStatus(t.state, t.pendingApprovalId, t.notBefore, t.waitReason)} />
        <span className="truncate">{t.input || <span className="muted">(empty input)</span>}</span>
        <span className="muted sub">{t.deployment}</span>
      </div>
      {node.children.map((c) => (
        <TaskTreeRow key={c.task.id} node={c} depth={depth + 1} currentId={currentId} onOpen={onOpen} />
      ))}
    </>
  );
}

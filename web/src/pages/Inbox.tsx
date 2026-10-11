import { CircleCheck, ExternalLink } from "lucide-react";
import { useEffect, useState } from "react";
import { Approval, call, Task, Transcript } from "../api";
import { inboxSeen, markInboxSeen, useSession } from "../App";
import { enumName } from "../format";
import { useQuery } from "../live";
import { deploymentHref, go, traceHref } from "../router";
import { Alert, Badge, Empty, Json, SkeletonRows, Status, taskStatus, Tabs, Time } from "../ui";
import { TaskPanel } from "./Tasks";

export function Inbox({ tab }: { tab?: string }) {
  const { can } = useSession();
  const [since] = useState(inboxSeen);
  useEffect(() => markInboxSeen(), []);
  const [openTask, setOpenTask] = useState<string>();
  const current = tab === "decided" ? "decided" : "open";
  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Inbox</h1>
          <p className="facts">Approvals agents wait for, and work that failed.</p>
        </div>
      </div>
      <Tabs
        value={current}
        onChange={(t) => go(["inbox", t === "open" ? undefined : t])}
        tabs={[
          ["open", "Needs you"],
          ["decided", "Decided approvals"],
        ]}
      />
      {current === "decided" ? (
        <DecidedApprovals />
      ) : (
        <>
          {can("approver") ? (
            <PendingApprovals />
          ) : (
            <Alert tone="info">Your token cannot decide approvals (it needs the approver scope).</Alert>
          )}
          <FailedTasks since={since} onOpen={setOpenTask} />
        </>
      )}
      {openTask && <TaskPanel id={openTask} onClose={() => setOpenTask(undefined)} />}
    </div>
  );
}

function PendingApprovals() {
  const { me } = useSession();
  const list = useQuery(() => call<{ approvals?: Approval[] }>("ListApprovals", { state: "APPROVAL_STATE_PENDING" }), [], { on: ["approval"] });
  const [msg, setMsg] = useState("");
  async function decide(id: string, approve: boolean) {
    try {
      await call("DecideApproval", { id, approve });
      setMsg("");
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    }
    list.reload();
  }
  const items = list.data?.approvals ?? [];
  return (
    <section className="section" aria-labelledby="pending">
      <h2 id="pending">Approvals {items.length > 0 && <Badge tone="warn">{items.length}</Badge>}</h2>
      {(list.error || msg) && <Alert>{list.error || msg}</Alert>}
      {list.loading ? (
        <SkeletonRows rows={2} cols={3} />
      ) : items.length === 0 ? (
        <p className="soft toolbar">
          <CircleCheck size={16} style={{ color: "var(--ok)" }} aria-hidden /> No approvals are waiting.
        </p>
      ) : (
        <div className="cards">
          {items.map((a) => (
            <ApprovalItem key={a.id} a={a} mine={a.requestedBy === me?.id} onDecide={decide} />
          ))}
        </div>
      )}
    </section>
  );
}

function ApprovalItem({ a, mine, onDecide }: { a: Approval; mine: boolean; onDecide: (id: string, approve: boolean) => void }) {
  const [context, setContext] = useState<Transcript>();
  const [open, setOpen] = useState(false);
  useEffect(() => {
    if (!open || context || !a.runId) return;
    call<Transcript>("GetTranscript", { runId: a.runId }).then(setContext, () => setContext({}));
  }, [open, context, a.runId]);
  const why = [...(context?.messages ?? [])].reverse().find((m) => m.role === "assistant" && m.content)?.content;
  return (
    <article className="approval" data-approval={a.id} aria-label={`${a.tool} for ${a.deployment}`}>
      <div className="approval-head">
        <div className="cell-main">
          <span>
            <b className="mono">{a.tool}</b> <span className="muted">for</span>{" "}
            <a href={deploymentHref(a.namespace, a.deployment)}>
              {a.namespace}/{a.deployment}
            </a>
          </span>
          <span className="sub">
            asked <Time t={a.createdAt} /> on behalf of {a.requestedByName || a.requestedBy || "unknown"} · expires <Time t={a.expiresAt} />
          </span>
        </div>
        <span data-field="state" className="sr-only">
          {enumName(a.state, "APPROVAL_STATE_")}
        </span>
        <div className="actions">
          <button className="btn danger" onClick={() => onDecide(a.id, false)} disabled={mine}>
            Deny
          </button>
          <button className="btn primary" onClick={() => onDecide(a.id, true)} disabled={mine}>
            Approve
          </button>
        </div>
      </div>
      {mine && <Alert tone="info">You started the work that asked for this, so nobody can approve it with your token: decide it with another token that has the approver scope.</Alert>}
      <Json value={a.arguments ?? {}} label={`arguments of ${a.tool}`} />
      <button className="btn ghost sm" onClick={() => setOpen(!open)} aria-expanded={open}>
        {open ? "Hide" : "Show"} what the agent was doing
      </button>
      {open && (
        <div className="section">
          {!context ? (
            <div className="skeleton" style={{ height: 40 }} />
          ) : (
            <>
              {context.run?.input && (
                <p>
                  <span className="muted">Task: </span>
                  {context.run.input}
                </p>
              )}
              {why && (
                <p>
                  <span className="muted">Agent: </span>
                  {why}
                </p>
              )}
              {context.run?.traceId && (
                <a className="btn sm" href={traceHref(context.run.traceId)}>
                  <ExternalLink size={13} aria-hidden />
                  Open the trace
                </a>
              )}
            </>
          )}
        </div>
      )}
    </article>
  );
}

function FailedTasks({ since, onOpen }: { since: number; onOpen: (id: string) => void }) {
  const failed = useQuery(() => call<{ tasks?: Task[] }>("ListTasks", { state: "TASK_STATE_FAILED", limit: 30 }), [], { on: ["task"] });
  const list = failed.data?.tasks ?? [];
  return (
    <section className="section" aria-labelledby="failed">
      <div className="section-head">
        <h2 id="failed">Failed tasks</h2>
        <span className="hint">New since your last visit are marked.</span>
      </div>
      {failed.error && <Alert>{failed.error}</Alert>}
      {failed.loading ? (
        <SkeletonRows rows={3} cols={3} />
      ) : list.length === 0 ? (
        <Empty title="No failed tasks" />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Task</th>
                <th>Error</th>
                <th>Failed</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.map((t) => (
                <tr key={t.id} className="clickable" onClick={() => onOpen(t.id)}>
                  <td>
                    <div className="cell-main">
                      <span className="toolbar nowrap">
                        <Status spec={taskStatus(t.state)}>{t.deployment}</Status>
                        {new Date(t.updatedAt ?? 0).getTime() > since && <Badge tone="err">new</Badge>}
                      </span>
                      <span className="sub truncate" style={{ maxWidth: 360 }}>
                        {t.input}
                      </span>
                    </div>
                  </td>
                  <td>
                    <span className="truncate" style={{ display: "block", maxWidth: 480, color: "var(--err)" }} title={t.error}>
                      {t.error}
                    </span>
                  </td>
                  <td>
                    <Time t={t.updatedAt} />
                  </td>
                  <td className="actions-cell">
                    {t.traceId && (
                      <a className="btn sm" href={traceHref(t.traceId)} onClick={(e) => e.stopPropagation()}>
                        Trace
                      </a>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function DecidedApprovals() {
  const list = useQuery(() => call<{ approvals?: Approval[] }>("ListApprovals", {}), [], { on: ["approval"] });
  const items = (list.data?.approvals ?? []).filter((a) => a.state !== "APPROVAL_STATE_PENDING");
  if (list.error) return <Alert>{list.error}</Alert>;
  if (list.loading) return <SkeletonRows rows={4} cols={5} />;
  if (!items.length) return <Empty title="No decided approvals yet" />;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Tool</th>
            <th>Deployment</th>
            <th>Decision</th>
            <th>By</th>
            <th>Asked by</th>
            <th>When</th>
          </tr>
        </thead>
        <tbody>
          {items.map((a) => {
            const state = enumName(a.state, "APPROVAL_STATE_");
            return (
              <tr key={a.id} data-approval={a.id}>
                <td className="mono">{a.tool}</td>
                <td>
                  {a.namespace}/{a.deployment}
                </td>
                <td data-field="state">
                  <Badge tone={state === "approved" ? "ok" : state === "denied" ? "err" : undefined}>{state}</Badge>
                </td>
                <td>{a.decidedByName || a.decidedBy || "–"}</td>
                <td>{a.requestedByName || a.requestedBy}</td>
                <td>
                  <Time t={a.decidedAt ?? a.expiresAt} />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

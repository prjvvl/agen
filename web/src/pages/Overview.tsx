import { AlertTriangle, CircleCheck, Coins, Inbox as InboxIcon, Plug, Server, XCircle } from "lucide-react";
import { ReactNode, useState } from "react";
import { Approval, call, Instance, Nest, Task } from "../api";
import { useSession } from "../App";
import { compact, money } from "../format";
import { LiveEvent, useLiveEvents, useQuery } from "../live";
import { deploymentHref } from "../router";
import { Alert, Empty, SkeletonRows, Status, taskStatus, Time } from "../ui";
import { depKey, useFleet } from "./Deployments";
import { FleetMap } from "./FleetMap";
import { RunsTable } from "./Runs";
import { TaskPanel } from "./Tasks";

interface Attention {
  key: string;
  icon: ReactNode;
  text: ReactNode;
  action: ReactNode;
}

export function Overview() {
  const { can } = useSession();
  const { deps, metrics, byDep } = useFleet();
  const approvals = useQuery(
    () => (can("approver") ? call<{ approvals?: Approval[] }>("ListApprovals", { state: "APPROVAL_STATE_PENDING" }) : Promise.resolve({ approvals: [] })),
    [can("approver")],
    { on: ["approval"] },
  );
  const instances = useQuery(() => call<{ instances?: Instance[] }>("ListInstances", {}), [], { on: ["instance"], every: 30_000 });
  const nests = useQuery(() => call<{ nests?: Nest[] }>("ListNests"), [], { on: ["instance", "deployment"], every: 30_000 });
  const [openTask, setOpenTask] = useState<string>();

  const list = deps.data?.deployments ?? [];
  const totals = (metrics.data?.deployments ?? []).reduce(
    (a, m) => ({ runs: a.runs + (m.runs ?? 0), failed: a.failed + (m.failed ?? 0), cost: a.cost + (m.usage?.costUsd ?? 0) }),
    { runs: 0, failed: 0, cost: 0 },
  );
  const busy = (instances.data?.instances ?? []).filter((i) => i.state.endsWith("READY") || i.state.endsWith("BUSY")).length;

  const attention: Attention[] = [];
  const pending = approvals.data?.approvals ?? [];
  if (pending.length)
    attention.push({
      key: "approvals",
      icon: <InboxIcon size={16} style={{ color: "var(--warn)" }} aria-hidden />,
      text: (
        <>
          <b>{pending.length}</b> {pending.length === 1 ? "approval waits" : "approvals wait"} for a decision
          <span className="muted"> · oldest from {pending[pending.length - 1].deployment}</span>
        </>
      ),
      action: (
        <a className="btn sm" href="#/inbox">
          Decide
        </a>
      ),
    });
  for (const m of metrics.data?.deployments ?? []) {
    if (!m.failed) continue;
    attention.push({
      key: "failed-" + depKey(m.ref),
      icon: <XCircle size={16} style={{ color: "var(--err)" }} aria-hidden />,
      text: (
        <>
          <b>{m.ref.name}</b>: {m.failed} of {m.runs} runs failed in the last 24 hours
        </>
      ),
      action: (
        <a className="btn sm" href={`#/runs?deployment=${encodeURIComponent(m.ref.name)}&status=failed&all=1`}>
          See failures
        </a>
      ),
    });
  }
  for (const d of list) {
    if (d.budgetExhausted)
      attention.push({
        key: "budget-" + depKey(d),
        icon: <Coins size={16} style={{ color: "var(--warn)" }} aria-hidden />,
        text: (
          <>
            <b>{d.name}</b> used its daily budget ({money(d.spentUsdToday)}); new work waits until tomorrow (UTC)
          </>
        ),
        action: (
          <a className="btn sm" href={deploymentHref(d.namespace, d.name)}>
            Open
          </a>
        ),
      });
  }
  const active = (nests.data?.nests ?? []).filter((n) => n.state.endsWith("ACTIVE"));
  const full = active.length > 0 && active.every((n) => (n.capacity ?? 0) > 0 && (n.used ?? 0) >= (n.capacity ?? 0));
  const waiting = list.filter((d) => !d.paused && (d.desired ?? 0) > (d.ready ?? 0));
  if (full && waiting.length)
    attention.push({
      key: "capacity",
      icon: <Server size={16} style={{ color: "var(--warn)" }} aria-hidden />,
      text: (
        <>
          <b>{waiting.map((d) => d.name).join(", ")}</b> {waiting.length === 1 ? "waits" : "wait"} for room: every nest is full (
          {active.map((n) => `${n.used}/${n.capacity}`).join(", ")} instances). Scale something down or add a nest.
        </>
      ),
      action: (
        <a className="btn sm" href="#/fleet">
          Fleet
        </a>
      ),
    });
  for (const i of instances.data?.instances ?? []) {
    const down = (i.toolServers ?? []).filter((s) => s.state !== "connected");
    if (i.state.endsWith("FAILED") || down.length)
      attention.push({
        key: "inst-" + i.id,
        icon: i.state.endsWith("FAILED") ? <AlertTriangle size={16} style={{ color: "var(--err)" }} aria-hidden /> : <Plug size={16} style={{ color: "var(--warn)" }} aria-hidden />,
        text: i.state.endsWith("FAILED") ? (
          <>
            An instance of <b>{i.deployment}</b> failed{i.message ? `: ${i.message}` : ""}
          </>
        ) : (
          <>
            <b>{i.deployment}</b>: tool server {down.map((s) => s.name).join(", ")} is not connected
          </>
        ),
        action: (
          <a className="btn sm" href={deploymentHref(i.namespace, i.deployment, "logs")}>
            Logs
          </a>
        ),
      });
  }

  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Overview</h1>
          <p className="facts">
            <span>
              <b>{list.length}</b> deployments
            </span>
            <span>
              <b>{busy}</b> instances up
            </span>
            <span>
              <b>{compact(totals.runs)}</b> runs in 24h
            </span>
            {totals.failed > 0 && (
              <span>
                <b>{totals.failed}</b> failed
              </span>
            )}
            <span>
              <b>{money(totals.cost)}</b> model spend in 24h
            </span>
          </p>
        </div>
      </div>

      <section className="section" aria-labelledby="attention">
        <h2 id="attention">Needs attention</h2>
        {attention.length === 0 ? (
          <p className="soft toolbar">
            <CircleCheck size={16} style={{ color: "var(--ok)" }} aria-hidden /> Nothing needs you right now.
          </p>
        ) : (
          <ul className="list">
            {attention.map((a) => (
              <li key={a.key}>
                {a.icon}
                <span className="grow" style={{ flex: 1, minWidth: 0 }}>
                  {a.text}
                </span>
                {a.action}
              </li>
            ))}
          </ul>
        )}
      </section>

      <div className="split">
        <section className="section" aria-labelledby="map">
          <div className="section-head">
            <h2 id="map">Fleet map</h2>
            <span className="hint">Who calls whom in the last 24 hours. Agents doing work right now are outlined.</span>
          </div>
          {deps.error && <Alert>{deps.error}</Alert>}
          {deps.loading ? (
            <div className="skeleton" style={{ height: 220 }} />
          ) : list.length === 0 ? (
            <Empty title="No deployments yet" action={<a className="btn primary" href="#/templates">Start from a template</a>}>
              Deployments appear here with the calls between them.
            </Empty>
          ) : (
            <FleetMap deps={list} edges={metrics.data?.edges ?? []} metrics={byDep} />
          )}
        </section>
        <section className="section" aria-labelledby="activity">
          <h2 id="activity">Activity</h2>
          <ActivityFeed onOpenTask={setOpenTask} />
        </section>
      </div>

      <section className="section" aria-labelledby="latest">
        <div className="section-head">
          <h2 id="latest">Latest traces</h2>
          <a className="more" href="#/runs">
            All runs
          </a>
        </div>
        <RunsTable filter={{}} limit={8} />
      </section>
      {openTask && <TaskPanel id={openTask} onClose={() => setOpenTask(undefined)} />}
    </div>
  );
}

/** Recent tasks, updated live as they change state. */
function ActivityFeed({ onOpenTask }: { onOpenTask: (id: string) => void }) {
  const tasks = useQuery(() => call<{ tasks?: Task[] }>("ListTasks", { limit: 12 }), [], { on: ["task", "approval"] });
  const [flash, setFlash] = useState<string>();
  useLiveEvents((e: LiveEvent) => {
    if (e.kind === "task") setFlash(e.id);
  });
  if (tasks.error) return <Alert>{tasks.error}</Alert>;
  if (tasks.loading) return <SkeletonRows rows={5} cols={2} />;
  const list = tasks.data?.tasks ?? [];
  if (!list.length) return <p className="muted">No tasks yet. They show up here as they are queued, run and finish.</p>;
  return (
    <ul className="list activity" aria-live="polite">
      {list.map((t) => (
        <li key={t.id} data-flash={t.id === flash || undefined}>
          <Status spec={taskStatus(t.state, t.pendingApprovalId)}>
            <span className="sr-only">{taskStatus(t.state, t.pendingApprovalId).label}</span>
          </Status>
          <button className="activity-text" onClick={() => onOpenTask(t.id)}>
            <span className="truncate">
              <b>{t.deployment}</b> <span className="soft">{t.input?.split("\n")[0]}</span>
            </span>
          </button>
          <Time t={t.updatedAt ?? t.createdAt} />
        </li>
      ))}
    </ul>
  );
}

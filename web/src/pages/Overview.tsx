import { useEffect, useState } from "react";
import { useActivity, useFlights } from "../activity";
import { ActiveRun, ActivityWarning, Approval, call, CallEdge, Deployment, DeploymentMetrics, Instance, Nest, num, QueueStatus, Task } from "../api";
import { useSession } from "../App";
import { compact, duration, money, tokens } from "../format";
import { LiveEvent, useLiveEvents, useQuery } from "../live";
import { deploymentHref, traceHref } from "../router";
import { Alert, Badge, deploymentStatus, Empty, MiniBars, Panel, SkeletonRows, Status, taskStatus, Time } from "../ui";
import { depKey, useFleet } from "./Deployments";
import { FleetMap, liveByDeployment } from "./FleetMap";
import { Doing, NowTable, useNow } from "./Now";
import { RunsTable } from "./Runs";
import { TaskPanel } from "./Tasks";
import { AttentionSummary, buildAttention } from "./Attention";
import { Timeline } from "./Timeline";

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
  const activity = useActivity();
  const flights = useFlights(activity.data);
  const live = liveByDeployment(activity.data?.runs, activity.data?.queues);
  const [selected, setSelected] = useState<{ kind: "node" | "edge"; key: string }>();

  const list = deps.data?.deployments ?? [];
  const totals = (metrics.data?.deployments ?? []).reduce(
    (a, m) => ({ runs: a.runs + (m.runs ?? 0), failed: a.failed + (m.failed ?? 0), cost: a.cost + (m.usage?.costUsd ?? 0) }),
    { runs: 0, failed: 0, cost: 0 },
  );
  const busy = (instances.data?.instances ?? []).filter((i) => i.state.endsWith("READY") || i.state.endsWith("BUSY")).length;

  const attention = buildAttention({ approvals, metrics, nests, instances, activity, list });

  const running = activity.data?.runs ?? [];
  const queued = (activity.data?.queues ?? []).reduce((a, q) => a + (q.queued ?? 0), 0);
  const flagged = new Set((activity.data?.warnings ?? []).map((w) => w.runId ?? "").filter(Boolean));
  const openDep = selected?.kind === "node" ? list.find((d) => depKey(d) === selected.key) : undefined;
  const openEdge = selected?.kind === "edge" ? (metrics.data?.edges ?? []).find((e) => `${depKey(e.from)}>${depKey(e.to)}` === selected.key) : undefined;

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
              <b>{running.length}</b> running now
            </span>
            {queued > 0 && (
              <span>
                <b>{queued}</b> queued
              </span>
            )}
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
        <AttentionSummary items={attention} />
      </section>

      <section className="section" aria-labelledby="map">
        <div className="section-head">
          <h2 id="map">Fleet map</h2>
          <span className="hint">Who calls whom in the last 24 hours. Dots travel while a call is in flight; select an agent or a line for details.</span>
        </div>
        {deps.error && <Alert>{deps.error}</Alert>}
        {deps.loading ? (
          <div className="skeleton" style={{ height: 220 }} />
        ) : list.length === 0 ? (
          <Empty title="No deployments yet" action={<a className="btn primary" href="#/templates">Start from a template</a>}>
            Deployments appear here with the calls between them.
          </Empty>
        ) : (
          <FleetMap
            deps={list}
            edges={metrics.data?.edges ?? []}
            metrics={byDep}
            live={live}
            flights={flights}
            onNode={(d) => setSelected({ kind: "node", key: depKey(d) })}
            onEdge={(e) => setSelected({ kind: "edge", key: `${depKey(e.from)}>${depKey(e.to)}` })}
          />
        )}
      </section>

      <div className="split">
        <section className="section" aria-labelledby="now">
          <div className="section-head">
            <h2 id="now">Now</h2>
            <span className="hint">Every run in progress and what it is doing; delegated runs sit under the run waiting for them.</span>
          </div>
          {activity.error && <Alert>{activity.error}</Alert>}
          {activity.loading ? <SkeletonRows rows={3} cols={5} /> : <NowTable runs={running} flagged={flagged} />}
        </section>
        <section className="section" aria-labelledby="activity">
          <h2 id="activity">Activity</h2>
          <ActivityFeed onOpenTask={setOpenTask} />
        </section>
      </div>

      <section className="section" aria-labelledby="timeline">
        <div className="section-head">
          <h2 id="timeline">Timeline</h2>
          <span className="hint">Runs per agent over time: overlaps, bursts and long runs at a glance.</span>
        </div>
        <Timeline active={running} />
      </section>

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
      {openDep && (
        <DeploymentLivePanel
          d={openDep}
          m={byDep.get(depKey(openDep))}
          runs={running.filter((r) => depKey(r.ref) === depKey(openDep))}
          queue={(activity.data?.queues ?? []).find((q) => depKey(q.ref) === depKey(openDep))}
          warnings={(activity.data?.warnings ?? []).filter((w) => w.ref && depKey(w.ref) === depKey(openDep))}
          onClose={() => setSelected(undefined)}
        />
      )}
      {openEdge && <EdgePanel e={openEdge} live={flights.filter((f) => !f.endedAt && f.from === depKey(openEdge.from) && f.to === depKey(openEdge.to)).length} onClose={() => setSelected(undefined)} />}
    </div>
  );
}


function DeploymentLivePanel({
  d,
  m,
  runs,
  queue,
  warnings,
  onClose,
}: {
  d: Deployment;
  m?: DeploymentMetrics;
  runs: ActiveRun[];
  queue?: QueueStatus;
  warnings: ActivityWarning[];
  onClose: () => void;
}) {
  const now = useNow();
  const st = deploymentStatus(d);
  return (
    <Panel
      title={d.name}
      onClose={onClose}
      footer={
        <>
          <a className="btn" href={deploymentHref(d.namespace, d.name)}>
            Open deployment
          </a>
          <a className="btn" href={`#/runs?deployment=${encodeURIComponent(d.name)}&all=1`}>
            Runs
          </a>
          <a className="btn" href={deploymentHref(d.namespace, d.name, "logs")}>
            Logs
          </a>
        </>
      }
    >
      <p>
        <Status spec={st}>{st.label}</Status> · {d.ready ?? 0}/{d.desired ?? 0} instances
        {queue?.queued ? ` · ${queue.queued} queued` : ""}
        {queue?.scheduled ? ` · ${queue.scheduled} scheduled` : ""}
      </p>
      {d.providerHold && (
        <Alert tone="err">
          Provider account {d.providerHold.account} is held: {d.providerHold.reason}. Next try <Time t={d.providerHold.retryAt} />.
        </Alert>
      )}
      {warnings.map((w) => (
        <Alert key={w.kind + w.message} tone={w.severity === "critical" ? "err" : "warn"}>
          {w.message}
        </Alert>
      ))}
      <h3>Running now</h3>
      {runs.length ? (
        <ul className="list">
          {runs.map((r) => (
            <li key={r.runId}>
              <Doing r={r} now={now} />
              <a className="grow truncate" href={r.traceId ? traceHref(r.traceId) : undefined} style={{ flex: 1, minWidth: 0 }}>
                {r.input?.split("\n")[0]}
              </a>
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted">Nothing running.</p>
      )}
      <h3>Last 24 hours</h3>
      <dl className="kv">
        <dt>Runs</dt>
        <dd>
          {m?.runs ?? 0}
          {m?.failed ? ` (${m.failed} failed)` : ""}
        </dd>
        <dt>p50 / p95</dt>
        <dd>
          {num(m?.p50Ms) ? `${duration(num(m?.p50Ms))} / ${duration(num(m?.p95Ms))}` : "–"}
        </dd>
        <dt>Tokens</dt>
        <dd>{compact(tokens(m?.usage))}</dd>
        <dt>Cost</dt>
        <dd>{money(m?.usage?.costUsd)}</dd>
      </dl>
      {m?.buckets && <MiniBars buckets={m.buckets} label={`${d.name} runs per hour`} />}
    </Panel>
  );
}

function EdgePanel({ e, live, onClose }: { e: CallEdge; live: number; onClose: () => void }) {
  return (
    <Panel
      title={`${e.from.name} → ${e.to.name}`}
      onClose={onClose}
      footer={
        <a className="btn" href={`#/runs?deployment=${encodeURIComponent(e.to.name)}&all=1`}>
          Runs of {e.to.name}
        </a>
      }
    >
      <dl className="kv">
        <dt>Calls (24h)</dt>
        <dd>{e.calls ?? 0}</dd>
        <dt>Failed</dt>
        <dd>
          {e.failed ?? 0}
          {e.calls ? ` (${Math.round(((e.failed ?? 0) / e.calls) * 100)}%)` : ""}
        </dd>
        <dt>Async (start_agent)</dt>
        <dd>{e.asyncCalls ?? 0}</dd>
        <dt>In flight</dt>
        <dd>{live || e.active || 0}</dd>
        <dt>p95 duration</dt>
        <dd>{num(e.p95Ms) ? duration(num(e.p95Ms)) : "—"}</dd>
      </dl>
    </Panel>
  );
}

/** Recent tasks, grouped by conversation, updated live as they change state. */
function ActivityFeed({ onOpenTask }: { onOpenTask: (id: string) => void }) {
  const tasks = useQuery(() => call<{ tasks?: Task[] }>("ListTasks", { limit: 30 }), [], { on: ["task", "approval"] });
  const [flash, setFlash] = useState<string>();
  const [hover, setHover] = useState(false);
  const [shown, setShown] = useState<Task[]>([]);
  useLiveEvents((e: LiveEvent) => {
    if (e.kind === "task") setFlash(e.id);
  });
  // Hold the list still while the pointer is on it.
  useEffect(() => {
    if (!hover && tasks.data) setShown(tasks.data.tasks ?? []);
  }, [tasks.data, hover]);
  if (tasks.error) return <Alert>{tasks.error}</Alert>;
  if (tasks.loading) return <SkeletonRows rows={5} cols={2} />;
  if (!shown.length) return <p className="muted">No tasks yet. They show up here as they are queued, run and finish.</p>;
  // Consecutive tasks of one conversation fold into one row.
  const groups: { lead: Task; count: number }[] = [];
  for (const t of shown) {
    const g = groups[groups.length - 1];
    if (g && t.conversationKey && g.lead.conversationKey === t.conversationKey && g.lead.deployment === t.deployment) g.count++;
    else groups.push({ lead: t, count: 1 });
  }
  return (
    <ul className="list activity" aria-live="polite" onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}>
      {groups.slice(0, 14).map(({ lead: t, count }) => (
        <li key={t.id} data-flash={t.id === flash || undefined}>
          <Status spec={taskStatus(t.state, t.pendingApprovalId)}>
            <span className="sr-only">{taskStatus(t.state, t.pendingApprovalId).label}</span>
          </Status>
          <button className="activity-text" onClick={() => onOpenTask(t.id)}>
            <span className="truncate">
              <b>{t.deployment}</b> <span className="soft">{t.input?.split("\n")[0]}</span>
            </span>
          </button>
          {count > 1 && <Badge title="Tasks in this conversation">×{count}</Badge>}
          {t.source && t.source !== "api" && <Badge title="Where the task came from">{t.source.split(":")[0]}</Badge>}
          <Time t={t.updatedAt ?? t.createdAt} />
        </li>
      ))}
    </ul>
  );
}

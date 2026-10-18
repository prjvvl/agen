import { AlertTriangle, ChevronLeft, ChevronRight, CircleCheck, Clock, Coins, Inbox as InboxIcon, Plug, Repeat, Server, XCircle } from "lucide-react";
import { ReactNode, useState } from "react";
import { useActivity } from "../activity";
import { Activity, ActivityWarning, Approval, call, Deployment, Instance, Metrics, Nest } from "../api";
import { useSession } from "../App";
import { money } from "../format";
import { useQuery } from "../live";
import { deploymentHref, traceHref } from "../router";
import { Alert, SkeletonRows } from "../ui";
import { depKey, useFleet } from "./Deployments";

/** Something that needs a person, with what to do about it. */
export interface Attention {
  key: string;
  icon: ReactNode;
  text: ReactNode;
  action: ReactNode;
  severity?: "critical" | "warning";
  /** What kind of thing it is (for the summary counts). */
  group?: string;
}

interface Sources {
  approvals: { data?: { approvals?: Approval[] } };
  metrics: { data?: Metrics };
  nests: { data?: { nests?: Nest[] } };
  instances: { data?: { instances?: Instance[] } };
  activity: { data?: Activity };
  list: Deployment[];
}

const GROUPS: [string, string][] = [
  ["approval", "approvals"],
  ["failed-", "failures"],
  ["budget-", "budgets"],
  ["capacity", "capacity"],
  ["inst-", "instances"],
];

// Group labels, singular and plural.
const GROUP_NAMES: Record<string, [string, string]> = {
  approvals: ["approval item", "approval items"],
  failures: ["deployment with failures", "deployments with failures"],
  budgets: ["budget used up", "budgets used up"],
  capacity: ["capacity problem", "capacity problems"],
  instances: ["instance problem", "instance problems"],
  near_turn_limit: ["run near its turn limit", "runs near their turn limit"],
  near_run_budget: ["run near its budget", "runs near their budget"],
  near_daily_budget: ["daily budget nearly used", "daily budgets nearly used"],
  quiet_run: ["quiet run", "quiet runs"],
  waiting_approval: ["long approval wait", "long approval waits"],
  queue_waiting: ["queue waiting", "queues waiting"],
  blocked_by_approval: ["queue blocked by an approval", "queues blocked by approvals"],
  burst: ["burst", "bursts"],
  model_errors: ["model error rate", "model error rates"],
  provider_held: ["held provider", "held providers"],
  trigger_missed: ["missed schedule", "missed schedules"],
  webhook_rejected: ["webhook refusing calls", "webhooks refusing calls"],
};

/** Everything that needs a person now, most serious first. */
export function buildAttention({ approvals, metrics, nests, instances, activity, list }: Sources): Attention[] {
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
          <span className="muted"> (calls from other agents included)</span>
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

  // Warnings from the Hub's live view (limits, quiet runs, bursts, models,
  // held provider accounts). Exhausted daily budgets are listed above.
  const runTrace = new Map((activity.data?.runs ?? []).map((r) => [r.runId, r.traceId]));
  for (const w of activity.data?.warnings ?? []) {
    if (w.kind === "near_daily_budget" && w.severity === "critical") continue;
    const dep = w.ref?.name ?? "";
    const trace = w.runId ? runTrace.get(w.runId) : undefined;
    attention.push({
      key: `w-${w.kind}-${dep}-${w.runId ?? ""}-${w.message}`,
      group: w.kind,
      severity: w.severity,
      icon: warnIcon(w),
      text: (
        <>
          <b>{dep}</b>: {w.message}
        </>
      ),
      action: trace ? (
        <a className="btn sm" href={traceHref(trace)}>
          Trace
        </a>
      ) : w.kind === "provider_held" ? (
        <a className="btn sm" href="#/costs">
          Providers
        </a>
      ) : (
        <a className="btn sm" href={deploymentHref(w.ref?.namespace ?? "default", dep)}>
          Open
        </a>
      ),
    });
  }

  for (const a of attention) {
    if (!a.group) a.group = GROUPS.find(([p]) => a.key.startsWith(p))?.[1] ?? "other";
    if (!a.severity) a.severity = a.key.startsWith("budget-") ? "critical" : "warning";
  }
  return attention.sort((a, b) => (a.severity === b.severity ? 0 : a.severity === "critical" ? -1 : 1));
}

function warnIcon(w: ActivityWarning) {
  const color = w.severity === "critical" ? "var(--err)" : "var(--warn)";
  const Icon = w.kind === "provider_held" || w.kind === "model_errors" ? Plug : w.kind === "burst" ? Repeat : w.kind.includes("budget") ? Coins : w.kind === "waiting_approval" ? InboxIcon : Clock;
  return <Icon size={16} style={{ color }} aria-hidden />;
}

/** The queries behind buildAttention, for pages that do not load them already. */
export function useAttention() {
  const { can } = useSession();
  const { deps, metrics } = useFleet();
  const approvals = useQuery(
    () => (can("approver") ? call<{ approvals?: Approval[] }>("ListApprovals", { state: "APPROVAL_STATE_PENDING" }) : Promise.resolve({ approvals: [] })),
    [can("approver")],
    { on: ["approval"] },
  );
  const instances = useQuery(() => call<{ instances?: Instance[] }>("ListInstances", {}), [], { on: ["instance"], every: 30_000 });
  const nests = useQuery(() => call<{ nests?: Nest[] }>("ListNests"), [], { on: ["instance", "deployment"], every: 30_000 });
  const activity = useActivity();
  const loading = deps.loading || activity.loading;
  return { items: buildAttention({ approvals, metrics, nests, instances, activity, list: deps.data?.deployments ?? [] }), loading, error: deps.error || activity.error };
}

function Row({ a }: { a: Attention }) {
  return (
    <li>
      {a.icon}
      <span className="grow" style={{ flex: 1, minWidth: 0 }}>
        {a.text}
      </span>
      {a.action}
    </li>
  );
}

/** Overview: what kinds of things need attention, and the most serious few. */
export function AttentionSummary({ items, top = 3 }: { items: Attention[]; top?: number }) {
  if (!items.length)
    return (
      <p className="soft toolbar">
        <CircleCheck size={16} style={{ color: "var(--ok)" }} aria-hidden /> Nothing needs you right now.
      </p>
    );
  const counts = new Map<string, { n: number; critical: boolean }>();
  for (const a of items) {
    const c = counts.get(a.group!) ?? { n: 0, critical: false };
    counts.set(a.group!, { n: c.n + 1, critical: c.critical || a.severity === "critical" });
  }
  return (
    <div className="attention">
      <div className="attention-chips">
        {[...counts.entries()].map(([g, c]) => (
          <a key={g} className="chip" data-tone={c.critical ? "err" : "warn"} href="#/inbox">
            <b>{c.n}</b> {(GROUP_NAMES[g] ?? [g, g])[c.n === 1 ? 0 : 1]}
          </a>
        ))}
      </div>
      <ul className="list">
        {items.slice(0, top).map((a) => (
          <Row key={a.key} a={a} />
        ))}
      </ul>
      {items.length > top && (
        <a className="more" href="#/inbox">
          See all {items.length} in the inbox
        </a>
      )}
    </div>
  );
}

/** Inbox: every item, a page at a time. */
export function AttentionList({ pageSize = 10 }: { pageSize?: number }) {
  const { items, loading, error } = useAttention();
  const [page, setPage] = useState(0);
  const pages = Math.max(1, Math.ceil(items.length / pageSize));
  const cur = Math.min(page, pages - 1);
  return (
    <section className="section" aria-labelledby="attention-all">
      <h2 id="attention-all">Needs attention</h2>
      {error && <Alert>{error}</Alert>}
      {loading ? (
        <SkeletonRows rows={3} cols={2} />
      ) : !items.length ? (
        <p className="soft toolbar">
          <CircleCheck size={16} style={{ color: "var(--ok)" }} aria-hidden /> Nothing needs you right now.
        </p>
      ) : (
        <>
          <ul className="list">
            {items.slice(cur * pageSize, (cur + 1) * pageSize).map((a) => (
              <Row key={a.key} a={a} />
            ))}
          </ul>
          {pages > 1 && (
            <div className="pager">
              <button className="btn sm" disabled={cur === 0} onClick={() => setPage(cur - 1)} aria-label="Previous page">
                <ChevronLeft size={14} />
              </button>
              <span className="muted">
                {cur * pageSize + 1}–{Math.min(items.length, (cur + 1) * pageSize)} of {items.length}
              </span>
              <button className="btn sm" disabled={cur >= pages - 1} onClick={() => setPage(cur + 1)} aria-label="Next page">
                <ChevronRight size={14} />
              </button>
            </div>
          )}
        </>
      )}
    </section>
  );
}


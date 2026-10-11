import { ChevronRight } from "lucide-react";
import { useEffect, useState } from "react";
import { call, Deployment, num, Run } from "../api";
import { compact, money, runDuration, tokens } from "../format";
import { useQuery } from "../live";
import { go, setQuery, traceHref } from "../router";
import { Alert, Badge, Empty, runStatus, Segmented, SkeletonRows, Status, Time } from "../ui";

export interface RunFilter {
  namespace?: string;
  deployment?: string;
  status?: string;
  label?: string;
  since?: string;
  all?: boolean;
}

const WINDOWS: Record<string, number> = { "1h": 3600, "24h": 86400, "7d": 7 * 86400 };

function request(f: RunFilter, pageToken?: string, limit = 50) {
  const labels: Record<string, string> = {};
  if (f.label?.includes("=")) {
    const [k, ...v] = f.label.split("=");
    labels[k.trim()] = v.join("=").trim();
  }
  return {
    namespace: f.namespace,
    deployment: f.deployment,
    status: f.status,
    rootsOnly: !f.all,
    labels: Object.keys(labels).length ? labels : undefined,
    since: f.since && WINDOWS[f.since] ? new Date(Date.now() - WINDOWS[f.since] * 1000).toISOString() : undefined,
    limit,
    pageToken,
  };
}

export function RunsTable({ filter, limit }: { filter: RunFilter; limit?: number }) {
  const key = JSON.stringify(filter) + limit;
  const first = useQuery(() => call<{ runs?: Run[]; nextPageToken?: string }>("ListRuns", request(filter, undefined, limit)), [key], {
    on: (e) => e.kind === "run" && (!filter.deployment || e.deployment === filter.deployment),
  });
  const [more, setMore] = useState<Run[]>([]);
  const [next, setNext] = useState<string>();
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    setMore([]);
    setNext(undefined);
  }, [key]);
  const token = limit ? undefined : more.length ? next : first.data?.nextPageToken;
  async function loadMore() {
    if (!token) return;
    setLoadingMore(true);
    try {
      const r = await call<{ runs?: Run[]; nextPageToken?: string }>("ListRuns", request(filter, token));
      setMore((m) => [...m, ...(r.runs ?? [])]);
      setNext(r.nextPageToken);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
    setLoadingMore(false);
  }
  if (first.error) return <Alert>{first.error}</Alert>;
  if (first.loading) return <SkeletonRows cols={6} />;
  const seen = new Set<string>();
  const runs = [...(first.data?.runs ?? []), ...more].filter((r) => !seen.has(r.id) && seen.add(r.id));
  if (!runs.length)
    return (
      <Empty title="No runs match">
        {filter.status || filter.label || filter.since ? "Try a wider filter." : "Runs appear here when tasks, triggers or other agents call this fleet."}
      </Empty>
    );
  const roots = !filter.all;
  return (
    <div className="section">
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Status</th>
              <th>Run</th>
              <th>Started</th>
              <th className="num">Duration</th>
              <th className="num">Steps</th>
              <th className="num">Tokens</th>
              <th className="num">Cost</th>
              {roots && <th className="num">Agents</th>}
              <th />
            </tr>
          </thead>
          <tbody>
            {runs.map((r) => {
              const u = roots && r.treeUsage ? r.treeUsage : r.usage;
              const open = () => r.traceId && go(["trace", r.traceId], { span: undefined });
              return (
                <tr key={r.id} data-run={r.id} className="clickable" onClick={open}>
                  <td>
                    <Status spec={runStatus(r.status)} />
                  </td>
                  <td>
                    <div className="cell-main">
                      <a href={r.traceId ? traceHref(r.traceId) : undefined} onClick={(e) => e.stopPropagation()} className="truncate" style={{ maxWidth: 420 }}>
                        {r.input?.split("\n")[0] || r.id}
                      </a>
                      <span className="sub">
                        {r.deployment}
                        {r.error ? <span style={{ color: "var(--err)" }}> · {r.error.slice(0, 120)}</span> : null}
                      </span>
                    </div>
                  </td>
                  <td>
                    <Time t={r.startedAt} />
                  </td>
                  <td className="num">{runDuration(r.startedAt, r.endedAt)}</td>
                  <td className="num">{num(r.steps) || "–"}</td>
                  <td className="num">{compact(tokens(u))}</td>
                  <td className="num">{u?.costUsd ? money(u.costUsd) : "–"}</td>
                  {roots && <td className="num">{(r.treeRuns ?? 1) > 1 ? <Badge>{r.treeRuns}</Badge> : 1}</td>}
                  <td className="actions-cell">
                    <ChevronRight size={15} className="muted" aria-hidden />
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {error && <Alert>{error}</Alert>}
      {token && (
        <div>
          <button className="btn" onClick={loadMore} disabled={loadingMore}>
            {loadingMore ? "Loading…" : "Load more"}
          </button>
        </div>
      )}
    </div>
  );
}

export function Runs({ query }: { query: URLSearchParams }) {
  const filter: RunFilter = {
    deployment: query.get("deployment") ?? undefined,
    status: query.get("status") ?? undefined,
    label: query.get("label") ?? undefined,
    since: query.get("since") ?? undefined,
    all: query.get("all") === "1",
  };
  const deps = useQuery(() => call<{ deployments?: Deployment[] }>("ListDeployments", {}), [], { on: ["deployment"] });
  const [label, setLabel] = useState(filter.label ?? "");
  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Runs</h1>
          <p className="facts">
            {filter.all
              ? "Every run, including runs other agents delegated."
              : "One row per trace: the run a task or call started, with the cost of every agent it called."}
          </p>
        </div>
      </div>
      <div className="toolbar" role="search">
        <Segmented
          label="Which runs"
          value={filter.all ? "all" : "traces"}
          onChange={(v) => setQuery({ all: v === "all" ? "1" : undefined })}
          options={[
            ["traces", "Traces"],
            ["all", "All runs"],
          ]}
        />
        <select value={filter.deployment ?? ""} onChange={(e) => setQuery({ deployment: e.target.value || undefined })} aria-label="Deployment">
          <option value="">All deployments</option>
          {(deps.data?.deployments ?? []).map((d) => (
            <option key={`${d.namespace}/${d.name}`} value={d.name}>
              {d.name}
            </option>
          ))}
        </select>
        <select value={filter.status ?? ""} onChange={(e) => setQuery({ status: e.target.value || undefined })} aria-label="Status">
          <option value="">Any status</option>
          <option value="running">Running</option>
          <option value="waiting_approval">Waiting for approval</option>
          <option value="succeeded">Succeeded</option>
          <option value="failed">Failed</option>
          <option value="cancelled">Cancelled</option>
        </select>
        <select value={filter.since ?? ""} onChange={(e) => setQuery({ since: e.target.value || undefined })} aria-label="Started">
          <option value="">Any time</option>
          <option value="1h">Last hour</option>
          <option value="24h">Last 24 hours</option>
          <option value="7d">Last 7 days</option>
        </select>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            setQuery({ label: label.trim() || undefined });
          }}
        >
          <input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="Label key=value" aria-label="Label" />
        </form>
      </div>
      <RunsTable filter={filter} />
    </div>
  );
}

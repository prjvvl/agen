import { LayoutTemplate, Minus, Plus } from "lucide-react";
import { useMemo, useState } from "react";
import { call, Deployment, DeploymentMetrics, Metrics, num } from "../api";
import { useSession } from "../App";
import { compact, duration, enumName, money, pct } from "../format";
import { useQuery } from "../live";
import { deploymentHref } from "../router";
import { Alert, deploymentStatus, Empty, MiniBars, SkeletonRows, Status, Time } from "../ui";

export const depKey = (r: { namespace?: string; name: string }) => `${r.namespace ?? "default"}/${r.name}`;

/** Deployments and their last-24h metrics, kept live. */
export function useFleet() {
  const deps = useQuery(() => call<{ deployments?: Deployment[] }>("ListDeployments", {}), [], { on: ["deployment", "instance"], every: 30_000 });
  const metrics = useQuery(() => call<Metrics>("GetMetrics", { windowSeconds: 86400, buckets: 24 }), [], { on: ["run"], every: 60_000 });
  const byDep = useMemo(() => {
    const m = new Map<string, DeploymentMetrics>();
    for (const d of metrics.data?.deployments ?? []) m.set(depKey(d.ref), d);
    return m;
  }, [metrics.data]);
  return { deps, metrics, byDep };
}

export function Deployments() {
  const { can } = useSession();
  const { deps, byDep } = useFleet();
  const [filter, setFilter] = useState("");
  const [actionError, setActionError] = useState("");
  const [pending, setPending] = useState(false);
  // One action at a time: a double click must not send the same step twice.
  const act = async (f: () => Promise<unknown>) => {
    if (pending) return;
    setPending(true);
    try {
      await f();
      setActionError("");
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    }
    await deps.reload();
    setPending(false);
  };
  const list = (deps.data?.deployments ?? []).filter((d) => depKey(d).includes(filter.trim().toLowerCase()));
  const operator = can("operator");
  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Deployments</h1>
          <p className="facts">Agents running from bundles. Metrics cover the last 24 hours.</p>
        </div>
        {operator && (
          <div className="actions">
            <a className="btn" href="#/templates">
              <LayoutTemplate size={15} aria-hidden />
              From a template
            </a>
            <a className="btn primary" href="#/new">
              <Plus size={15} aria-hidden />
              New agent
            </a>
          </div>
        )}
      </div>
      {(deps.error || actionError) && <Alert>{deps.error || actionError}</Alert>}
      {deps.loading ? (
        <SkeletonRows cols={7} />
      ) : (deps.data?.deployments ?? []).length === 0 ? (
        <Empty
          title="No deployments yet"
          action={
            operator && (
              <div className="actions">
                <a className="btn primary" href="#/templates">
                  Start from a template
                </a>
                <a className="btn" href="#/new">
                  Write one
                </a>
              </div>
            )
          }
        >
          Deploy an agent from a template, write one here, or run <code>agen deploy</code>.
        </Empty>
      ) : (
        <>
          <div className="toolbar">
            <input type="search" placeholder="Filter by name" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter deployments" />
          </div>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Deployment</th>
                  <th>Status</th>
                  <th>Instances</th>
                  <th>Runs (24h)</th>
                  <th className="num">Failed</th>
                  <th className="num">p95</th>
                  <th className="num">Cost today</th>
                  <th>Last active</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {list.map((d) => {
                  const m = byDep.get(depKey(d));
                  const ref = { namespace: d.namespace, name: d.name };
                  const min = d.scale?.min ?? 0;
                  const max = d.scale?.max ?? 1;
                  const desired = d.desired ?? 0;
                  const scale = (n: number) => act(() => call("ScaleDeployment", { ref, desired: n }));
                  const last = num(d.lastActivityUnix);
                  return (
                    <tr key={depKey(d)} data-deployment={d.name}>
                      <td>
                        <div className="cell-main">
                          <a href={deploymentHref(d.namespace, d.name)}>{d.name}</a>
                          <span className="sub">
                            {d.namespace} · {enumName(d.kind, "DEPLOYMENT_KIND_")}
                          </span>
                        </div>
                      </td>
                      <td data-field="status">
                        <Status spec={deploymentStatus(d)} />
                      </td>
                      <td>
                        <div className="toolbar nowrap">
                          {operator && (
                            <button
                              className="btn sm icon"
                              aria-label={`scale ${d.name} down`}
                              disabled={pending || d.paused || desired <= min}
                              onClick={() => scale(desired - 1)}
                            >
                              <Minus size={13} />
                            </button>
                          )}
                          <span className="num" title={`ready ${d.ready ?? 0}, desired ${desired}, scale ${min}..${max}`}>
                            <span data-field="ready">{d.ready ?? 0}</span>
                            <span className="muted"> / </span>
                            <span data-field="desired">{desired}</span>
                          </span>
                          {operator && (
                            <button
                              className="btn sm icon"
                              aria-label={`scale ${d.name} up`}
                              disabled={pending || d.paused || desired >= max}
                              onClick={() => scale(desired + 1)}
                            >
                              <Plus size={13} />
                            </button>
                          )}
                        </div>
                      </td>
                      <td>
                        <div className="toolbar nowrap">
                          <MiniBars buckets={m?.buckets} label={`${d.name} runs per hour`} />
                          <span className="num">{compact(m?.runs)}</span>
                        </div>
                      </td>
                      <td className="num">{m?.failed ? <a href={`#/runs?deployment=${d.name}&status=failed`}>{pct(m.failed, m.runs ?? 0)}</a> : "–"}</td>
                      <td className="num">{m && num(m.p95Ms) ? duration(num(m.p95Ms)) : "–"}</td>
                      <td className="num">
                        {money(d.spentUsdToday)}
                        {d.budget?.maxUsdPerDay ? <span className="muted"> / {money(d.budget.maxUsdPerDay)}</span> : null}
                      </td>
                      <td>{last ? <Time t={new Date(last * 1000).toISOString()} /> : <span className="muted">never</span>}</td>
                      <td className="actions-cell">
                        {operator && (
                          <button className="btn sm" disabled={pending} onClick={() => act(() => call("PauseDeployment", { ref, paused: !d.paused }))}>
                            {d.paused ? "Start" : "Stop"}
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}

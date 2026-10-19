import { useMemo, useState } from "react";
import { CircleCheck, XCircle } from "lucide-react";
import { call, Deployment, Metrics, ModelMetrics, num, ProviderHold } from "../api";
import { useSession } from "../App";
import { compact, duration, money, pct } from "../format";
import { useQuery } from "../live";
import { deploymentHref } from "../router";
import { Alert, Empty, Segmented, SkeletonRows, Time } from "../ui";
import { depKey } from "./Deployments";

const WINDOWS = { "24h": [86400, 24], "7d": [7 * 86400, 28], "30d": [30 * 86400, 30] } as const;
type Win = keyof typeof WINDOWS;
const SERIES = ["var(--kind-run)", "var(--kind-model)", "var(--kind-tool)", "var(--kind-agent)", "var(--kind-wait)"];

export function Costs() {
  const [win, setWin] = useState<Win>("24h");
  const [seconds, buckets] = WINDOWS[win];
  const metrics = useQuery(() => call<Metrics>("GetMetrics", { windowSeconds: seconds, buckets }), [win], { on: ["run"], every: 60_000 });
  const deps = useQuery(() => call<{ deployments?: Deployment[] }>("ListDeployments", {}), [], { on: ["deployment"] });
  const budgets = new Map((deps.data?.deployments ?? []).map((d) => [depKey(d), d]));
  const rows = useMemo(() => [...(metrics.data?.deployments ?? [])].sort((a, b) => (b.usage?.costUsd ?? 0) - (a.usage?.costUsd ?? 0) || (b.runs ?? 0) - (a.runs ?? 0)), [metrics.data]);
  const total = rows.reduce(
    (a, m) => ({
      cost: a.cost + (m.usage?.costUsd ?? 0),
      runs: a.runs + (m.runs ?? 0),
      failed: a.failed + (m.failed ?? 0),
      tin: a.tin + num(m.usage?.inputTokens),
      tout: a.tout + num(m.usage?.outputTokens),
    }),
    { cost: 0, runs: 0, failed: 0, tin: 0, tout: 0 },
  );

  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Costs</h1>
          <p className="facts">
            <span>
              <b>{money(total.cost)}</b> model spend
            </span>
            <span>
              <b>{compact(total.tin)}</b> input and <b>{compact(total.tout)}</b> output tokens
            </span>
            <span>
              <b>{compact(total.runs)}</b> runs
            </span>
          </p>
        </div>
        <Segmented
          label="Time window"
          value={win}
          onChange={setWin}
          options={[
            ["24h", "24 hours"],
            ["7d", "7 days"],
            ["30d", "30 days"],
          ]}
        />
      </div>
      {metrics.error && <Alert>{metrics.error}</Alert>}
      {metrics.loading ? (
        <SkeletonRows cols={6} />
      ) : rows.length === 0 ? (
        <Empty title="No runs in this window">Cost shows up once agents run. Providers that report cost (OpenRouter does) fill the dollar columns.</Empty>
      ) : (
        <>
          <CostChart metrics={metrics.data!} />
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Deployment</th>
                  <th className="num">Runs</th>
                  <th className="num">Failed</th>
                  <th className="num">Median</th>
                  <th className="num">p95</th>
                  <th className="num">Tokens in</th>
                  <th className="num">Tokens out</th>
                  <th className="num">Cost</th>
                  <th style={{ width: 120 }}>Share</th>
                  <th className="num">Today / daily budget</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((m) => {
                  const d = budgets.get(depKey(m.ref));
                  const cost = m.usage?.costUsd ?? 0;
                  return (
                    <tr key={depKey(m.ref)}>
                      <td>
                        <a href={deploymentHref(m.ref.namespace ?? "default", m.ref.name)}>{m.ref.name}</a>
                      </td>
                      <td className="num">{compact(m.runs)}</td>
                      <td className="num">{m.failed ? pct(m.failed, m.runs ?? 0) : "–"}</td>
                      <td className="num">{num(m.p50Ms) ? duration(num(m.p50Ms)) : "–"}</td>
                      <td className="num">{num(m.p95Ms) ? duration(num(m.p95Ms)) : "–"}</td>
                      <td className="num">{compact(m.usage?.inputTokens)}</td>
                      <td className="num">{compact(m.usage?.outputTokens)}</td>
                      <td className="num">{money(cost)}</td>
                      <td>
                        <div className="share" title={pct(cost, total.cost)}>
                          <span style={{ width: `${total.cost ? (cost / total.cost) * 100 : 0}%` }} />
                        </div>
                      </td>
                      <td className="num">
                        {money(d?.spentUsdToday)}
                        <span className="muted"> / {d?.budget?.maxUsdPerDay ? money(d.budget.maxUsdPerDay) : "none"}</span>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <ModelsSection models={metrics.data?.models ?? []} />
        </>
      )}
      <ProviderHolds />
    </div>
  );
}

/** Model calls by the model that served them (routers and aliases resolved). */
function ModelsSection({ models }: { models: ModelMetrics[] }) {
  return (
    <section className="section" aria-labelledby="models">
      <div className="section-head">
        <h2 id="models">Models</h2>
        <span className="hint">Every model call, by the model that actually answered it: a routing model shows here as the models it picked.</span>
      </div>
      {!models.length ? (
        <Empty title="No model calls in this window" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Model</th>
                <th className="num">Calls</th>
                <th className="num">Failed</th>
                <th className="num">Fallbacks</th>
                <th className="num">p50 / p95</th>
                <th className="num">Tokens in / out</th>
                <th className="num">Cost</th>
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={m.model}>
                  <td>
                    <code>{m.model}</code>
                    {m.provider && <span className="muted"> · {m.provider}</span>}
                  </td>
                  <td className="num">{compact(m.calls)}</td>
                  <td className="num" data-tone={(m.failed ?? 0) > 0 ? "err" : undefined}>
                    {m.failed ?? 0}
                    {m.calls ? <span className="muted"> ({pct(m.failed ?? 0, m.calls)})</span> : null}
                  </td>
                  <td className="num">{m.fallbacks ?? 0}</td>
                  <td className="num">
                    {duration(num(m.p50Ms))} / {duration(num(m.p95Ms))}
                  </td>
                  <td className="num">
                    {compact(num(m.usage?.inputTokens))} / {compact(num(m.usage?.outputTokens))}
                  </td>
                  <td className="num">{money(m.usage?.costUsd)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

/** Provider accounts held after an account error, with a way to resume. */
function ProviderHolds() {
  const { can } = useSession();
  const holds = useQuery(() => call<{ holds?: ProviderHold[] }>("ListProviderHolds", {}), [], { on: ["task", "deployment"], every: 30_000 });
  const [err, setErr] = useState("");
  const list = holds.data?.holds ?? [];
  return (
    <section className="section" aria-labelledby="providers">
      <div className="section-head">
        <h2 id="providers">Provider accounts</h2>
        <span className="hint">An invalid key or used-up credit holds every agent on that account; one task probes it after a backoff.</span>
      </div>
      {err && <Alert>{err}</Alert>}
      {holds.error && <Alert>{holds.error}</Alert>}
      {!list.length ? (
        <p className="soft toolbar">
          <CircleCheck size={16} style={{ color: "var(--ok)" }} aria-hidden /> Every provider account works.
        </p>
      ) : (
        <ul className="list">
          {list.map((h) => (
            <li key={h.account}>
              <XCircle size={16} style={{ color: "var(--err)" }} aria-hidden />
              <span className="grow" style={{ flex: 1, minWidth: 0 }}>
                <b>{h.account}</b> held since <Time t={h.heldAt} />: {h.reason}
                <span className="muted">
                  {" "}
                  · next try <Time t={h.retryAt} /> · {h.failures} {h.failures === 1 ? "failure" : "failures"}
                </span>
              </span>
              {can("admin") && (
                <button
                  className="btn sm"
                  onClick={() =>
                    call("ResumeProvider", { account: h.account })
                      .then(() => (setErr(""), holds.reload()))
                      .catch((e) => setErr(e instanceof Error ? e.message : String(e)))
                  }
                >
                  Resume now
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** Spend per time bucket, stacked by the top deployments. */
function CostChart({ metrics }: { metrics: Metrics }) {
  const deps = [...(metrics.deployments ?? [])].sort((a, b) => (b.usage?.costUsd ?? 0) - (a.usage?.costUsd ?? 0));
  const byCost = deps.some((d) => (d.usage?.costUsd ?? 0) > 0);
  const top = deps.slice(0, 8);
  const rest = deps.slice(4);
  const n = deps[0]?.buckets?.length ?? 0;
  const value = (b?: { costUsd?: number; runs?: number }) => (byCost ? (b?.costUsd ?? 0) : (b?.runs ?? 0));
  const series = [
    ...top.map((d, i) => ({ name: d.ref.name, color: SERIES[i], values: Array.from({ length: n }, (_, k) => value(d.buckets?.[k])) })),
    ...(rest.length
      ? [{ name: `${rest.length} more`, color: "var(--fg-muted)", values: Array.from({ length: n }, (_, k) => rest.reduce((a, d) => a + value(d.buckets?.[k]), 0)) }]
      : []),
  ];
  const sums = Array.from({ length: n }, (_, k) => series.reduce((a, s) => a + s.values[k], 0));
  const max = Math.max(...sums, byCost ? 0.0001 : 1);
  const W = 1000;
  const H = 160;
  const bw = W / Math.max(n, 1);
  const since = metrics.since ? new Date(metrics.since).getTime() : Date.now();
  const step = (metrics.bucketSeconds ?? 3600) * 1000;
  const label = (k: number) => {
    const d = new Date(since + k * step);
    return step < 86400_000 ? d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" }) : d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  };
  return (
    <section className="section chart" aria-labelledby="spend-chart">
      <div className="section-head">
        <h2 id="spend-chart">{byCost ? "Spend over time" : "Runs over time"}</h2>
        <span className="hint">{byCost ? `Peak ${money(max)} per bucket.` : `Peak ${max} runs per bucket. No provider reported cost in this window.`}</span>
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label={`${byCost ? "Spend" : "Runs"} per time bucket`} style={{ width: "100%", height: 180 }}>
        <line x1={0} x2={W} y1={H} y2={H} className="chart-axis" />
        {sums.map((_, k) => {
          let y = H;
          return (
            <g key={k}>
              <title>
                {label(k)}: {byCost ? money(sums[k]) : `${sums[k]} runs`}
              </title>
              {series.map((s) => {
                const h = (s.values[k] / max) * (H - 8);
                y -= h;
                return h > 0 ? <rect key={s.name} x={k * bw + 1} y={y} width={Math.max(bw - 2, 1)} height={h} fill={s.color} /> : null;
              })}
            </g>
          );
        })}
      </svg>
      <div className="chart-x" aria-hidden>
        <span>{label(0)}</span>
        <span>{label(Math.floor(n / 2))}</span>
        <span>now</span>
      </div>
      <div className="legend toolbar">
        {series.map((s) => (
          <span key={s.name}>
            <i style={{ background: s.color }} />
            {s.name}
          </span>
        ))}
      </div>
    </section>
  );
}

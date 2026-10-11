import { useMemo, useState } from "react";
import { call, Deployment, Metrics, num } from "../api";
import { compact, duration, money, pct } from "../format";
import { useQuery } from "../live";
import { deploymentHref } from "../router";
import { Alert, Empty, Segmented, SkeletonRows } from "../ui";
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
        </>
      )}
    </div>
  );
}

/** Spend per time bucket, stacked by the top deployments. */
function CostChart({ metrics }: { metrics: Metrics }) {
  const deps = [...(metrics.deployments ?? [])].sort((a, b) => (b.usage?.costUsd ?? 0) - (a.usage?.costUsd ?? 0));
  const byCost = deps.some((d) => (d.usage?.costUsd ?? 0) > 0);
  const top = deps.slice(0, 4);
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
        <span>{label(n - 1)}</span>
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

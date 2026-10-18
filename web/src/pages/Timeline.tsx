import { useEffect, useMemo, useRef, useState } from "react";
import { ActiveRun, call, Run } from "../api";
import { duration, money } from "../format";
import { useQuery } from "../live";
import { traceHref } from "../router";
import { Alert, Empty, Segmented, SkeletonRows } from "../ui";
import { useNow } from "./Now";

type Window = "15m" | "1h" | "6h" | "24h";
const WINDOWS: Record<Window, number> = { "15m": 15 * 60_000, "1h": 3_600_000, "6h": 6 * 3_600_000, "24h": 24 * 3_600_000 };
const LABEL_W = 148;
const ROW_H = 22;

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(800);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver((e) => setWidth(Math.max(320, e[0].contentRect.width)));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  return { ref, width };
}

const tone = (status: string) =>
  status === "succeeded" ? "ok" : status === "failed" ? "err" : status === "cancelled" ? "muted" : status === "waiting_approval" ? "wait" : "run";

/**
 * Runs per agent on a time axis (one row per agent, one bar per run, the way
 * Temporal shows a workflow's history): overlaps, bursts and long waits are
 * visible at a glance. Running bars grow to now.
 */
export function Timeline({ active = [] }: { active?: ActiveRun[] }) {
  const [win, setWin] = useState<Window>("1h");
  const [chosen, setChosen] = useState(false);
  const now = useNow(5000);
  const span = WINDOWS[win];
  const runs = useQuery(
    () => call<{ runs?: Run[] }>("ListRuns", { limit: 500, since: new Date(Date.now() - span).toISOString() }),
    [win],
    { on: ["run"], every: 30_000 },
  );
  const { ref, width } = useWidth<HTMLDivElement>();
  const start = now - span;
  const rows = useMemo(() => {
    // Runs that started in the window, plus unfinished runs that started
    // before it (they still occupy it).
    const all = new Map<string, Run>((runs.data?.runs ?? []).map((r) => [r.id, r]));
    for (const a of active)
      if (!all.has(a.runId))
        all.set(a.runId, {
          id: a.runId,
          namespace: a.ref.namespace ?? "default",
          deployment: a.ref.name,
          status: a.activity === "approval" ? "waiting_approval" : "running",
          startedAt: a.startedAt,
          traceId: a.traceId,
          input: a.input,
          usage: a.usage,
        });
    const m = new Map<string, Run[]>();
    for (const r of all.values()) m.set(r.deployment, [...(m.get(r.deployment) ?? []), r]);
    return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  }, [runs.data, active]);
  const plot = width - LABEL_W - 8;
  const x = (t: number) => LABEL_W + ((Math.max(t, start) - start) / span) * plot;
  const ticks = Array.from({ length: 5 }, (_, i) => start + (span * i) / 4);
  // Until the operator picks a window: a young fleet's runs fit in 15 minutes.
  useEffect(() => {
    if (chosen || win !== "1h" || !runs.data?.runs?.length) return;
    const first = Math.min(...runs.data.runs.map((r) => Date.parse(r.startedAt ?? "") || now));
    if (now - first < WINDOWS["15m"] * 0.9) setWin("15m");
  }, [runs.data, chosen, win, now]);
  const truncated = (runs.data?.runs?.length ?? 0) >= 500;

  return (
    <div className="timeline" ref={ref}>
      <div className="section-tools">
        <Segmented<Window>
          label="Time window"
          value={win}
          onChange={(w) => {
            setChosen(true);
            setWin(w);
          }}
          options={[
            ["15m", "15 min"],
            ["1h", "1 hour"],
            ["6h", "6 hours"],
            ["24h", "24 hours"],
          ]}
        />
        <span className="map-legend" aria-hidden>
          <span className="lg-bar" data-tone="ok" /> succeeded
          <span className="lg-bar" data-tone="err" /> failed
          <span className="lg-bar" data-tone="run" /> running
          <span className="lg-bar" data-tone="wait" /> waiting for approval
        </span>
      </div>
      {runs.error && <Alert>{runs.error}</Alert>}
      {runs.loading ? (
        <SkeletonRows rows={4} cols={1} />
      ) : !rows.length ? (
        <Empty title="No runs in this window">Each run shows up as a bar on its agent's row.</Empty>
      ) : (
        <svg width={width} height={rows.length * ROW_H + 26} role="img" aria-label={`Runs of ${rows.length} agents over the last ${win}`}>
          {ticks.map((t) => (
            <g key={t} className="tl-tick">
              <line x1={x(t)} x2={x(t)} y1={0} y2={rows.length * ROW_H} />
              <text x={x(t)} y={rows.length * ROW_H + 16} textAnchor="middle">
                {new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
              </text>
            </g>
          ))}
          {rows.map(([dep, list], i) => (
            <g key={dep} transform={`translate(0 ${i * ROW_H})`}>
              <text x={0} y={ROW_H / 2 + 4} className="tl-label">
                {dep.length > 20 ? dep.slice(0, 19) + "…" : dep}
              </text>
              <line className="tl-row" x1={LABEL_W} x2={width - 8} y1={ROW_H - 0.5} y2={ROW_H - 0.5} />
              {list.map((r) => {
                const s = Date.parse(r.startedAt ?? "") || now;
                const e = Date.parse(r.endedAt ?? "") || now;
                const x1 = x(s);
                const w = Math.max(3, x(e) - x1);
                return (
                  <a key={r.id} href={r.traceId ? traceHref(r.traceId) : undefined}>
                    <rect className="tl-bar" data-tone={tone(r.status)} x={x1} y={4} width={w} height={ROW_H - 8} rx={2}>
                      <title>
                        {`${dep} · ${r.status} · ${duration(e - s)}${r.usage?.costUsd ? ` · ${money(r.usage.costUsd)}` : ""}\n${(r.input ?? "").slice(0, 160)}`}
                      </title>
                    </rect>
                  </a>
                );
              })}
            </g>
          ))}
          <line className="tl-now" x1={x(now)} x2={x(now)} y1={0} y2={rows.length * ROW_H} />
        </svg>
      )}
      {truncated && <p className="muted small">Showing the latest 500 runs of this window.</p>}
    </div>
  );
}

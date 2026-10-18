import { useMemo, useState } from "react";
import { ActiveRun, CallEdge, Deployment, DeploymentMetrics, num, QueueStatus } from "../api";
import { Flight } from "../activity";
import { compact, duration, money } from "../format";
import { deploymentStatus, Segmented } from "../ui";
import { depKey } from "./Deployments";

const W = 212;
const H = 74;
const GAP_X = 112;
const GAP_Y = 26;

interface Placed {
  d: Deployment;
  x: number;
  y: number;
}

/** Layers deployments left to right along call edges (callers before callees). */
function layout(deps: Deployment[], edges: { from: string; to: string }[]): { nodes: Placed[]; width: number; height: number } {
  const keys = deps.map(depKey);
  const rank = new Map(keys.map((k) => [k, 0]));
  const known = edges.filter((e) => rank.has(e.from) && rank.has(e.to) && e.from !== e.to);
  // Longest path from the callers; the pass limit stops at cycles.
  for (let pass = 0; pass < keys.length; pass++) {
    let changed = false;
    for (const e of known) {
      const r = rank.get(e.from)! + 1;
      if (r > rank.get(e.to)! && r < keys.length) {
        rank.set(e.to, r);
        changed = true;
      }
    }
    if (!changed) break;
  }
  const connected = new Set(known.flatMap((e) => [e.from, e.to]));
  const columns: Deployment[][] = [];
  const alone: Deployment[] = [];
  for (const d of deps) {
    if (connected.has(depKey(d))) (columns[rank.get(depKey(d))!] ??= []).push(d);
    else alone.push(d);
  }
  columns.forEach((col) => col.sort((a, b) => a.name.localeCompare(b.name)));
  const nodes: Placed[] = [];
  columns.forEach((col, c) => col.forEach((d, r) => nodes.push({ d, x: 12 + c * (W + GAP_X), y: 12 + r * (H + GAP_Y) })));
  // Deployments that call nobody and are called by nobody: a grid below.
  const graphRows = Math.max(0, ...columns.map((c) => c.length));
  const perRow = Math.max(3, columns.length * 2 - 1);
  const top = 12 + graphRows * (H + GAP_Y) + (graphRows ? 16 : 0);
  alone
    .sort((a, b) => a.name.localeCompare(b.name))
    .forEach((d, i) => nodes.push({ d, x: 12 + (i % perRow) * (W + 24), y: top + Math.floor(i / perRow) * (H + GAP_Y) }));
  const width = 12 + Math.max(...nodes.map((n) => n.x + W));
  const height = 12 + Math.max(...nodes.map((n) => n.y + H));
  return { nodes, width, height };
}

export type EdgeLabel = "calls" | "errors" | "latency";

/** What a node shows about the agent right now. */
export interface NodeLive {
  running: number;
  thinking: number;
  waiting: number;
  queued: number;
}

export function liveByDeployment(runs: ActiveRun[] = [], queues: QueueStatus[] = []): Map<string, NodeLive> {
  const m = new Map<string, NodeLive>();
  const get = (k: string) => m.get(k) ?? (m.set(k, { running: 0, thinking: 0, waiting: 0, queued: 0 }), m.get(k)!);
  for (const r of runs) {
    const n = get(depKey(r.ref));
    n.running++;
    if (r.activity === "model") n.thinking++;
    if (r.activity === "approval" || r.activity === "agent") n.waiting++;
  }
  for (const q of queues) get(depKey(q.ref)).queued += q.queued ?? 0;
  return m;
}

/** Arc of a ring from fraction a to b (0..1, clockwise from the top). */
function arc(cx: number, cy: number, r: number, a: number, b: number) {
  const p = (t: number) => [cx + r * Math.sin(2 * Math.PI * t), cy - r * Math.cos(2 * Math.PI * t)];
  const [x1, y1] = p(a);
  const [x2, y2] = p(Math.min(b, a + 0.9999));
  return `M${x1} ${y1} A${r} ${r} 0 ${b - a > 0.5 ? 1 : 0} 1 ${x2} ${y2}`;
}

interface MapProps {
  deps: Deployment[];
  edges: CallEdge[];
  metrics: Map<string, DeploymentMetrics>;
  live: Map<string, NodeLive>;
  flights: Flight[];
  onNode: (d: Deployment) => void;
  onEdge: (e: CallEdge) => void;
}

/**
 * Deployments as a graph of who calls whom (Kiali-style traffic): edge width
 * follows the number of calls, colour the share that failed, dashes mark
 * async calls; dots travel along an edge while a call is in flight and come
 * back (a red diamond when it failed). Nodes ring their success share and
 * pulse while a model call runs.
 */
export function FleetMap({ deps, edges, metrics, live, flights, onNode, onEdge }: MapProps) {
  const [label, setLabel] = useState<EdgeLabel>("calls");
  const reduced = useMemo(() => typeof matchMedia !== "undefined" && matchMedia("(prefers-reduced-motion: reduce)").matches, []);
  // Edges from metrics plus calls in flight that metrics have not seen yet.
  const allEdges = useMemo(() => {
    const out = new Map<string, CallEdge>();
    for (const e of edges) out.set(`${depKey(e.from)}>${depKey(e.to)}`, e);
    for (const f of flights) {
      const k = `${f.from}>${f.to}`;
      if (!out.has(k)) {
        const [fns, fname] = f.from.split("/");
        const [tns, tname] = f.to.split("/");
        out.set(k, { from: { namespace: fns, name: fname }, to: { namespace: tns, name: tname }, calls: 0 });
      }
    }
    return [...out.values()];
  }, [edges, flights]);
  const { nodes, width, height } = useMemo(
    () => layout(deps, allEdges.map((e) => ({ from: depKey(e.from), to: depKey(e.to) }))),
    [deps, allEdges],
  );
  const at = new Map(nodes.map((n) => [depKey(n.d), n]));
  const flightsOn = new Map<string, Flight[]>();
  for (const f of flights) {
    const k = `${f.from}>${f.to}`;
    flightsOn.set(k, [...(flightsOn.get(k) ?? []), f]);
  }

  const edgePath = (a: Placed, b: Placed) => {
    const x1 = a.x + W;
    const y1 = a.y + H / 2;
    const x2 = b.x;
    const y2 = b.y + H / 2;
    if (x2 <= x1) {
      // Back edge: an arc above the row.
      const lift = 70 + Math.abs(y1 - y2) / 3;
      return { d: `M${a.x + W / 2} ${a.y} C${a.x + W / 2} ${a.y - lift}, ${b.x + W / 2} ${b.y - lift}, ${b.x + W / 2} ${b.y}`, mx: (a.x + b.x + W) / 2, my: Math.min(a.y, b.y) - lift * 0.75 };
    }
    const mid = (x1 + x2) / 2;
    return { d: `M${x1} ${y1} C${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`, mx: mid, my: (y1 + y2) / 2 };
  };

  return (
    <div className="map-wrap">
      <div className="map-tools">
        <Segmented<EdgeLabel>
          label="Edge labels"
          value={label}
          onChange={setLabel}
          options={[
            ["calls", "Calls"],
            ["errors", "Errors"],
            ["latency", "Latency"],
          ]}
        />
        <span className="map-legend" aria-hidden>
          <span className="lg-line" /> calls (24h)
          <span className="lg-line dashed" /> async
          <span className="lg-dot" /> in flight
          <span className="lg-diamond" /> failed
        </span>
      </div>
      <div className="map" role="group" aria-label={`Map of ${deps.length} deployments and ${allEdges.length} call paths`}>
        <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`}>
          <defs>
            <marker id="arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
              <path d="M0 0 8 4 0 8z" className="map-arrow" />
            </marker>
          </defs>
          {allEdges.map((e) => {
            const k = `${depKey(e.from)}>${depKey(e.to)}`;
            const a = at.get(depKey(e.from));
            const b = at.get(depKey(e.to));
            if (!a || !b || a === b) return null;
            const { d, mx, my } = edgePath(a, b);
            const calls = e.calls ?? 0;
            const failed = e.failed ?? 0;
            const share = calls ? failed / calls : 0;
            const tone = share >= 0.2 ? "err" : failed > 0 ? "warn" : undefined;
            const inFlight = flightsOn.get(k) ?? [];
            const live = inFlight.filter((f) => !f.endedAt).length;
            const async = (e.asyncCalls ?? 0) > 0 && (e.asyncCalls ?? 0) >= calls / 2;
            const text =
              label === "errors"
                ? failed
                  ? `${failed} failed · ${Math.round(share * 100)}%`
                  : "no errors"
                : label === "latency"
                  ? num(e.p95Ms)
                    ? `p95 ${duration(num(e.p95Ms))}`
                    : "—"
                  : `${compact(calls)} ${calls === 1 ? "call" : "calls"}`;
            return (
              <g
                key={k}
                className="map-edge"
                data-tone={tone}
                data-async={async || undefined}
                data-live={live > 0 || undefined}
                role="button"
                tabIndex={0}
                aria-label={`${e.from.name} calls ${e.to.name}: ${calls} calls, ${failed} failed${live ? `, ${live} in flight` : ""}`}
                onClick={() => onEdge(e)}
                onKeyDown={(ev) => (ev.key === "Enter" || ev.key === " ") && (ev.preventDefault(), onEdge(e))}
              >
                <path className="hit" d={d} />
                <path d={d} markerEnd="url(#arrow)" style={{ strokeWidth: Math.min(6, 1.25 + Math.log2(1 + calls) * 0.6) }} />
                <text x={mx} y={my - 7} textAnchor="middle">
                  {text}
                  {live > 0 ? ` · ${live} live` : ""}
                </text>
                {inFlight.map((f, i) =>
                  reduced ? (
                    <circle key={f.key} r={4} className="map-flight" data-failed={f.failed || undefined} cx={mx} cy={my + 8 + i * 9} />
                  ) : f.endedAt ? (
                    // Coming back with the answer (a red diamond if it failed).
                    <rect key={f.key} className="map-flight" data-failed={f.failed || undefined} x={-4} y={-4} width={8} height={8} rx={f.failed ? 0 : 4} transform={f.failed ? "rotate(45)" : undefined}>
                      <animateMotion dur="1.2s" repeatCount="1" fill="freeze" path={d} keyPoints="1;0" keyTimes="0;1" calcMode="linear" />
                    </rect>
                  ) : (
                    <circle key={f.key} r={4} className="map-flight">
                      <animateMotion dur="1.6s" begin={`${(i * 0.4) % 1.6}s`} repeatCount="indefinite" path={d} />
                    </circle>
                  ),
                )}
              </g>
            );
          })}
          {nodes.map(({ d, x, y }) => {
            const k = depKey(d);
            const st = deploymentStatus(d);
            const m = metrics.get(k);
            const l = live.get(k);
            const runs = m?.runs ?? 0;
            const failShare = runs ? (m?.failed ?? 0) / runs : 0;
            const asleep = !l?.running && !(d.ready ?? 0) && !(d.desired ?? 0);
            const now = l?.running
              ? `${l.running} running${l.waiting ? ` (${l.waiting} waiting)` : ""}${l.queued ? ` · ${l.queued} queued` : ""}`
              : l?.queued
                ? `${l.queued} queued · ${st.label}`
                : st.label;
            const cx = x + W - 22;
            const cy = y + 22;
            return (
              <g
                key={k}
                className="map-node"
                data-tone={d.providerHold ? "err" : st.tone}
                data-live={(l?.running ?? 0) > 0 || undefined}
                data-thinking={(l?.thinking ?? 0) > 0 || undefined}
                data-asleep={asleep || undefined}
                data-held={d.providerHold ? true : undefined}
                role="button"
                tabIndex={0}
                aria-label={`${d.name}: ${now}`}
                onClick={() => onNode(d)}
                onKeyDown={(ev) => (ev.key === "Enter" || ev.key === " ") && (ev.preventDefault(), onNode(d))}
              >
                <rect x={x} y={y} width={W} height={H} rx={8} />
                <circle cx={x + 16} cy={y + 20} r={4} className="map-dot" />
                <text x={x + 28} y={y + 24} className="map-name">
                  {d.name.length > 20 ? d.name.slice(0, 19) + "…" : d.name}
                </text>
                <text x={x + 16} y={y + 44} className="map-now">
                  {d.providerHold ? "provider held" : now}
                </text>
                <text x={x + 16} y={y + 61} className="map-sub">
                  {compact(runs)} runs{m?.failed ? ` · ${m.failed} failed` : ""}
                  {m?.usage?.costUsd ? ` · ${money(m.usage.costUsd)}` : ""}
                </text>
                {runs > 0 && (
                  <g className="map-ring" aria-hidden>
                    <circle cx={cx} cy={cy} r={9} className="ring-track" />
                    <path d={arc(cx, cy, 9, 0, 1 - failShare)} className="ring-ok" />
                    {failShare > 0 && <path d={arc(cx, cy, 9, 1 - failShare, 1)} className="ring-err" />}
                  </g>
                )}
              </g>
            );
          })}
        </svg>
      </div>
    </div>
  );
}

import { useMemo, useState } from "react";
import { CallEdge, Deployment, DeploymentMetrics } from "../api";
import { compact } from "../format";
import { useLiveEvents } from "../live";
import { deploymentHref } from "../router";
import { deploymentStatus } from "../ui";
import { depKey } from "./Deployments";

const W = 196;
const H = 60;
const GAP_X = 96;
const GAP_Y = 22;

interface Placed {
  d: Deployment;
  x: number;
  y: number;
}

/** Layers deployments left to right along call edges (callers before callees). */
function layout(deps: Deployment[], edges: CallEdge[]): { nodes: Placed[]; width: number; height: number } {
  const keys = deps.map(depKey);
  const rank = new Map(keys.map((k) => [k, 0]));
  const known = edges.filter((e) => rank.has(depKey(e.from)) && rank.has(depKey(e.to)) && depKey(e.from) !== depKey(e.to));
  // Longest path from the callers; the pass limit stops at cycles.
  for (let pass = 0; pass < keys.length; pass++) {
    let changed = false;
    for (const e of known) {
      const r = rank.get(depKey(e.from))! + 1;
      if (r > rank.get(depKey(e.to))! && r < keys.length) {
        rank.set(depKey(e.to), r);
        changed = true;
      }
    }
    if (!changed) break;
  }
  const connected = new Set(known.flatMap((e) => [depKey(e.from), depKey(e.to)]));
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
  const top = 12 + graphRows * (H + GAP_Y) + (graphRows ? 12 : 0);
  alone
    .sort((a, b) => a.name.localeCompare(b.name))
    .forEach((d, i) => nodes.push({ d, x: 12 + (i % perRow) * (W + 24), y: top + Math.floor(i / perRow) * (H + GAP_Y) }));
  const width = 12 + Math.max(...nodes.map((n) => n.x + W));
  const height = 12 + Math.max(...nodes.map((n) => n.y + H));
  return { nodes, width, height };
}

/** Deployments as a graph of who calls whom, with live activity. */
export function FleetMap({ deps, edges, metrics }: { deps: Deployment[]; edges: CallEdge[]; metrics: Map<string, DeploymentMetrics> }) {
  const [active, setActive] = useState<Map<string, number>>(new Map());
  useLiveEvents((e) => {
    if (e.kind !== "run" && e.kind !== "task") return;
    const k = `${e.namespace}/${e.deployment}`;
    setActive((m) => new Map(m).set(k, Date.now()));
    setTimeout(() => setActive((m) => new Map(m)), 6000);
  });
  const { nodes, width, height } = useMemo(() => layout(deps, edges), [deps, edges]);
  const at = new Map(nodes.map((n) => [depKey(n.d), n]));
  const isActive = (k: string) => Date.now() - (active.get(k) ?? 0) < 5000;

  return (
    <div className="map" role="img" aria-label={`Map of ${deps.length} deployments and ${edges.length} call paths`}>
      <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`}>
        <defs>
          <marker id="arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
            <path d="M0 0 8 4 0 8z" className="map-arrow" />
          </marker>
        </defs>
        {edges.map((e) => {
          const a = at.get(depKey(e.from));
          const b = at.get(depKey(e.to));
          if (!a || !b || a === b) return null;
          const x1 = a.x + W;
          const y1 = a.y + H / 2;
          const x2 = b.x;
          const y2 = b.y + H / 2;
          const back = x2 <= x1;
          const mid = (x1 + x2) / 2;
          const d = back
            ? `M${x1} ${y1} C${x1 + 60} ${y1 - 70}, ${x2 - 60} ${y2 - 70}, ${x2} ${y2}`
            : `M${x1} ${y1} C${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`;
          const live = isActive(depKey(e.from)) && isActive(depKey(e.to));
          return (
            <g key={`${depKey(e.from)}>${depKey(e.to)}`} className="map-edge" data-failed={(e.failed ?? 0) > 0 || undefined} data-live={live || undefined}>
              <path d={d} markerEnd="url(#arrow)" />
              <text x={mid} y={(y1 + y2) / 2 - 6} textAnchor="middle">
                {compact(e.calls)} {e.calls === 1 ? "call" : "calls"}
                {e.failed ? `, ${e.failed} failed` : ""}
              </text>
            </g>
          );
        })}
        {nodes.map(({ d, x, y }) => {
          const k = depKey(d);
          const st = deploymentStatus(d);
          const m = metrics.get(k);
          return (
            <a key={k} href={deploymentHref(d.namespace, d.name)} className="map-node" data-tone={st.tone} data-live={isActive(k) || undefined} aria-label={`${d.name}: ${st.label}`}>
              <rect x={x} y={y} width={W} height={H} rx={8} />
              <circle cx={x + 16} cy={y + 20} r={4} className="map-dot" />
              <text x={x + 28} y={y + 24} className="map-name">
                {d.name.length > 22 ? d.name.slice(0, 21) + "…" : d.name}
              </text>
              <text x={x + 16} y={y + 44} className="map-sub">
                {st.label} · {compact(m?.runs)} runs{m?.failed ? ` · ${m.failed} failed` : ""}
              </text>
            </a>
          );
        })}
      </svg>
    </div>
  );
}

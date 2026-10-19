import { Maximize2, Minus, Plus } from "lucide-react";
import { PointerEvent as ReactPointerEvent, useEffect, useMemo, useRef, useState } from "react";
import { ActiveRun, CallEdge, Deployment, DeploymentMetrics, EntryEdge, num, QueueStatus } from "../api";
import { Flight } from "../activity";
import { compact, duration, money } from "../format";
import { deploymentStatus, Segmented } from "../ui";
import { depKey } from "./Deployments";

// Card size: full, or compact for a big fleet.
const FULL = { w: 212, h: 74 };
const COMPACT = { w: 176, h: 52 };
/** Above this many agents the map uses compact cards and hides quiet ones. */
const BIG_FLEET = 12;
const FRAME_H = 560;
const EW = 132; // entry node width
const EH = 44;
const GAP_X = 104;
const GAP_Y = 24;

/** Where work comes from that no agent of the fleet sent. */
const ENTRIES: Record<string, string> = {
  "entry:api": "API & CLI",
  "entry:cron": "Schedules",
  "entry:webhook": "Webhooks",
  "entry:a2a": "Outside callers",
};

export type EdgeKind = "call" | "async" | "reply" | "entry";

/** One line on the map. */
export interface MapEdge {
  key: string;
  from: string;
  to: string;
  kind: EdgeKind;
  calls: number;
  failed: number;
  asyncCalls?: number;
  active?: number;
  p95Ms?: number;
  /** The metrics row for agent-to-agent calls (side panel). */
  call?: CallEdge;
}

interface MapNode {
  key: string;
  d?: Deployment;
  entry?: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

/** Entry nodes in the first column, then agents layered along call edges. */
function layout(deps: Deployment[], entryKeys: string[], edges: MapEdge[], size = FULL, frameWidth = 0): { nodes: MapNode[]; width: number; height: number } {
  const W = size.w;
  const H = size.h;
  const maxRows = size === COMPACT ? 9 : 7;
  const keys = deps.map(depKey);
  const rank = new Map(keys.map((k) => [k, 0]));
  const calls = edges.filter((e) => (e.kind === "call" || e.kind === "async") && rank.has(e.from) && rank.has(e.to) && e.from !== e.to);
  for (let pass = 0; pass < keys.length; pass++) {
    let changed = false;
    for (const e of calls) {
      const r = rank.get(e.from)! + 1;
      if (r > rank.get(e.to)! && r < keys.length) {
        rank.set(e.to, r);
        changed = true;
      }
    }
    if (!changed) break;
  }
  const connected = new Set(edges.flatMap((e) => [e.from, e.to]));
  const columns: Deployment[][] = [];
  const alone: Deployment[] = [];
  for (const d of deps) {
    if (connected.has(depKey(d))) (columns[rank.get(depKey(d))!] ??= []).push(d);
    else alone.push(d);
  }
  columns.forEach((col) => col.sort((a, b) => a.name.localeCompare(b.name)));
  // A tall column wraps into several side by side (a fan-out of 20 agents
  // stays on screen).
  const wrapped: Deployment[][] = [];
  for (const col of columns) for (let i = 0; i < (col?.length ?? 0); i += maxRows) wrapped.push(col.slice(i, i + maxRows));
  const nodes: MapNode[] = [];
  const left = entryKeys.length ? 12 + EW + GAP_X : 12;
  entryKeys.forEach((k, i) => nodes.push({ key: k, entry: ENTRIES[k] ?? k, x: 12, y: 12 + i * (EH + GAP_Y) + (H - EH) / 2, w: EW, h: EH }));
  wrapped.forEach((col, c) => col.forEach((d, r) => nodes.push({ key: depKey(d), d, x: left + c * (W + GAP_X), y: 12 + r * (H + GAP_Y), w: W, h: H })));
  const rows = Math.max(entryKeys.length ? Math.ceil((entryKeys.length * (EH + GAP_Y)) / (H + GAP_Y)) : 0, ...wrapped.map((c) => c.length));
  // Agents without lines: a grid as wide as the frame (or the graph).
  const graphWidth = left + wrapped.length * (W + GAP_X);
  const perRow = Math.max(3, Math.floor((Math.max(frameWidth, graphWidth) - 12) / (W + 24)));
  const top = 12 + rows * (H + GAP_Y) + (rows ? 16 : 0);
  alone
    .sort((a, b) => a.name.localeCompare(b.name))
    .forEach((d, i) => nodes.push({ key: depKey(d), d, x: 12 + (i % perRow) * (W + 24), y: top + Math.floor(i / perRow) * (H + GAP_Y), w: W, h: H }));
  const width = 12 + Math.max(...nodes.map((n) => n.x + n.w));
  const height = 12 + Math.max(...nodes.map((n) => n.y + n.h));
  return { nodes, width, height };
}

/** What a node shows about the agent right now. */
export interface NodeLive {
  running: number;
  thinking: number;
  approvals: number;
  waitingAgent: number;
  queued: number;
  scheduled: number;
}

export function liveByDeployment(runs: ActiveRun[] = [], queues: QueueStatus[] = []): Map<string, NodeLive> {
  const m = new Map<string, NodeLive>();
  const get = (k: string) => m.get(k) ?? (m.set(k, { running: 0, thinking: 0, approvals: 0, waitingAgent: 0, queued: 0, scheduled: 0 }), m.get(k)!);
  for (const r of runs) {
    const n = get(depKey(r.ref));
    n.running++;
    if (r.activity === "model") n.thinking++;
    if (r.activity === "approval") n.approvals++;
    if (r.activity === "agent") n.waitingAgent++;
  }
  for (const q of queues) {
    get(depKey(q.ref)).queued += q.queued ?? 0;
    get(depKey(q.ref)).scheduled += q.scheduled ?? 0;
  }
  return m;
}

/** All the lines: calls between agents, entries, results going back, and anything in flight not counted yet. */
export function mapEdges(calls: CallEdge[], entries: EntryEdge[], flights: Flight[]): MapEdge[] {
  const out = new Map<string, MapEdge>();
  for (const e of calls) {
    const from = depKey(e.from);
    const to = depKey(e.to);
    const asyncCalls = e.asyncCalls ?? 0;
    out.set(`${from}>${to}`, {
      key: `${from}>${to}`,
      from,
      to,
      kind: asyncCalls > 0 && asyncCalls >= (e.calls ?? 0) / 2 ? "async" : "call",
      calls: e.calls ?? 0,
      failed: e.failed ?? 0,
      asyncCalls,
      active: e.active,
      p95Ms: num(e.p95Ms),
      call: e,
    });
  }
  for (const e of entries) {
    const to = depKey(e.to);
    const from = e.source === "reply" && e.from ? depKey(e.from) : `entry:${e.source}`;
    const kind: EdgeKind = e.source === "reply" ? "reply" : "entry";
    const k = `${from}>${to}${kind === "reply" ? ":reply" : ""}`;
    out.set(k, { key: k, from, to, kind, calls: e.runs ?? 0, failed: e.failed ?? 0 });
  }
  for (const f of flights) {
    const k = `${f.from}>${f.to}${f.kind === "reply" ? ":reply" : ""}`;
    if (!out.has(k)) out.set(k, { key: k, from: f.from, to: f.to, kind: f.kind, calls: 0, failed: 0 });
  }
  return [...out.values()];
}

/** Arc of a ring from fraction a to b (0..1, clockwise from the top). */
function arc(cx: number, cy: number, r: number, a: number, b: number) {
  const p = (t: number) => [cx + r * Math.sin(2 * Math.PI * t), cy - r * Math.cos(2 * Math.PI * t)];
  const [x1, y1] = p(a);
  const [x2, y2] = p(Math.min(b, a + 0.9999));
  return `M${x1} ${y1} A${r} ${r} 0 ${b - a > 0.5 ? 1 : 0} 1 ${x2} ${y2}`;
}

const plural = (n: number, one: string, many = one + "s") => `${n} ${n === 1 ? one : many}`;
const cut = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1) + "…" : s);
const clipId = (key: string) => `clip-${key.replace(/[^a-zA-Z0-9_-]/g, "_")}`;

export type EdgeLabel = "calls" | "errors" | "latency";

interface MapProps {
  deps: Deployment[];
  edges: MapEdge[];
  metrics: Map<string, DeploymentMetrics>;
  live: Map<string, NodeLive>;
  flights: Flight[];
  onNode: (d: Deployment) => void;
  onEdge: (e: MapEdge) => void;
}

/**
 * The fleet as a traffic map (in the spirit of Kiali): where work enters
 * (API, schedules, webhooks, outside callers), who calls whom (solid), who
 * starts whom without waiting (dashed), and results going back (dotted).
 * Line width follows the number of runs, colour the share that failed. Dots
 * travel while work is in flight and come back when it ends (a red diamond
 * when it failed). Agents ring their success share and pulse during a model
 * call.
 */
export function FleetMap({ deps: allDeps, edges: allEdges, metrics, live, flights, onNode, onEdge }: MapProps) {
  const [label, setLabel] = useState<EdgeLabel>("calls");
  const reduced = useMemo(() => typeof matchMedia !== "undefined" && matchMedia("(prefers-reduced-motion: reduce)").matches, []);
  const big = allDeps.length > BIG_FLEET;
  const size = big ? COMPACT : FULL;
  // Quiet agents (nothing in 24h, nothing running or waiting, no lines) can
  // be hidden on a big fleet.
  const [showQuiet, setShowQuiet] = useState(true);
  const quiet = useMemo(() => {
    const touched = new Set(allEdges.flatMap((e) => [e.from, e.to]));
    return new Set(
      allDeps
        .filter((d) => {
          const k = depKey(d);
          const l = live.get(k);
          return !touched.has(k) && !(metrics.get(k)?.runs ?? 0) && !l?.running && !l?.queued && !l?.scheduled && !d.providerHold;
        })
        .map(depKey),
    );
  }, [allDeps, allEdges, metrics, live]);
  const hideQuiet = big && !showQuiet && quiet.size > 0;
  const deps = hideQuiet ? allDeps.filter((d) => !quiet.has(depKey(d))) : allDeps;
  const edges = allEdges;
  // Find: the matching agents and their direct neighbours stand out.
  const [find, setFind] = useState("");
  const focus = useMemo(() => {
    const q = find.trim().toLowerCase();
    if (!q) return undefined;
    const hit = new Set(allDeps.filter((d) => d.name.toLowerCase().includes(q)).map(depKey));
    const near = new Set(hit);
    for (const e of allEdges) {
      if (hit.has(e.from)) near.add(e.to);
      if (hit.has(e.to)) near.add(e.from);
    }
    return { hit, near };
  }, [find, allDeps, allEdges]);
  const entryKeys = useMemo(() => Object.keys(ENTRIES).filter((k) => edges.some((e) => e.from === k)), [edges]);
  const frame = useRef<HTMLDivElement>(null);
  const [frameWidth, setFrameWidth] = useState(0);
  useEffect(() => {
    if (!frame.current) return;
    const ro = new ResizeObserver((e) => setFrameWidth(Math.round(e[0].contentRect.width / 50) * 50));
    ro.observe(frame.current);
    return () => ro.disconnect();
  }, []);
  const { nodes, width, height } = useMemo(() => layout(deps, entryKeys, edges, size, frameWidth - 24), [deps, entryKeys, edges, size, frameWidth]);
  const W = size.w;
  const H = size.h;
  // Pan and zoom inside a fixed frame once the map is taller than it.
  const frameH = Math.min(FRAME_H, height + 52);
  const [view, setView] = useState({ k: 1, x: 0, y: 40 });
  const fit = () => {
    const fw = frame.current?.clientWidth ?? width;
    const k = Math.min(1, (fw - 16) / width, (frameH - 16) / (height + 40));
    setView({ k, x: 8, y: 8 + 40 * k });
  };
  const fitted = useRef(false);
  useEffect(() => {
    if (fitted.current || !nodes.length || !frameWidth) return;
    fitted.current = true;
    if (height + 52 > FRAME_H || width > (frame.current?.clientWidth ?? width)) fit();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nodes.length, frameWidth]);
  const zoom = (f: number, cx = (frame.current?.clientWidth ?? 0) / 2, cy = frameH / 2) =>
    setView((v) => {
      const k = Math.min(2, Math.max(0.3, v.k * f));
      return { k, x: cx - ((cx - v.x) * k) / v.k, y: cy - ((cy - v.y) * k) / v.k };
    });
  const drag = useRef<{ x: number; y: number; vx: number; vy: number } | null>(null);
  const onDown = (e: ReactPointerEvent) => {
    if ((e.target as Element).closest(".map-node, .map-edge")) return;
    drag.current = { x: e.clientX, y: e.clientY, vx: view.x, vy: view.y };
    (e.currentTarget as Element).setPointerCapture(e.pointerId);
  };
  const onMove = (e: ReactPointerEvent) => {
    const d = drag.current;
    if (d) setView((v) => ({ ...v, x: d.vx + e.clientX - d.x, y: d.vy + e.clientY - d.y }));
  };
  useEffect(() => {
    const el = frame.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      // Ctrl/Cmd + wheel zooms; the plain wheel keeps scrolling the page
      // (drag to pan).
      if (e.ctrlKey || e.metaKey) {
        e.preventDefault();
        const r = el.getBoundingClientRect();
        zoom(e.deltaY < 0 ? 1.15 : 1 / 1.15, e.clientX - r.left, e.clientY - r.top);
      }
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [height, width, frameH]);
  const at = new Map(nodes.map((n) => [n.key, n]));
  // Lines into one node get their labels one above the other.
  const labelRow = new Map<string, number>();
  const into = new Map<string, MapEdge[]>();
  // (entry and result lines show their label on hover only)
  for (const e of edges) if (e.kind !== "entry" && e.kind !== "reply") into.set(e.to, [...(into.get(e.to) ?? []), e]);
  for (const list of into.values()) {
    list.sort((a, b) => (at.get(a.from)?.y ?? 0) - (at.get(b.from)?.y ?? 0));
    list.forEach((e, i) => labelRow.set(e.key, i - (list.length - 1) / 2));
  }
  const flightsOn = new Map<string, Flight[]>();
  for (const f of flights) {
    const k = `${f.from}>${f.to}${f.kind === "reply" ? ":reply" : ""}`;
    flightsOn.set(k, [...(flightsOn.get(k) ?? []), f]);
  }

  const edgePath = (a: MapNode, b: MapNode) => {
    if (a === b) {
      // An agent calling itself: a loop over its top right corner.
      const x = a.x + a.w - 44;
      return { d: `M${x} ${a.y} C${x - 10} ${a.y - 44}, ${x + 54} ${a.y - 44}, ${a.x + a.w} ${a.y + 14}`, mx: x + 22, my: a.y - 30 };
    }
    const x1 = a.x + a.w;
    const y1 = a.y + a.h / 2;
    const x2 = b.x;
    const y2 = b.y + b.h / 2;
    if (x2 <= x1) {
      // Backwards (a result going back, or a call to an earlier column): an
      // arc above the row.
      const lift = 60 + Math.abs(y1 - y2) / 3;
      return {
        d: `M${a.x + a.w / 2} ${a.y} C${a.x + a.w / 2} ${a.y - lift}, ${b.x + b.w / 2} ${b.y - lift}, ${b.x + b.w / 2 + 24} ${b.y}`,
        mx: (a.x + b.x + a.w) / 2,
        my: Math.min(a.y, b.y) - lift * 0.72,
      };
    }
    const mid = (x1 + x2) / 2;
    // The label sits just before the target, where edges into different
    // targets are apart (at the midpoint, edges from one node bunch up).
    return { d: `M${x1} ${y1} C${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`, mx: x2 - Math.min(56, (x2 - x1) / 3), my: y2 };
  };

  return (
    <div className="map-wrap">
      <div className="map-tools">
        <Segmented<EdgeLabel>
          label="Edge labels"
          value={label}
          onChange={setLabel}
          options={[
            ["calls", "Runs"],
            ["errors", "Errors"],
            ["latency", "Latency"],
          ]}
        />
        <input className="map-find" type="search" placeholder="Find an agent" aria-label="Find an agent" value={find} onChange={(e) => setFind(e.target.value)} />
        {big && quiet.size > 0 && (
          <button className="btn sm" onClick={() => setShowQuiet((v) => !v)} aria-pressed={showQuiet}>
            {showQuiet ? "Hide" : "Show"} {quiet.size} quiet {quiet.size === 1 ? "agent" : "agents"}
          </button>
        )}
        <span className="map-zoom">
          <button className="btn sm icon" aria-label="Zoom in" onClick={() => zoom(1.2)}>
            <Plus size={14} />
          </button>
          <button className="btn sm icon" aria-label="Zoom out" onClick={() => zoom(1 / 1.2)}>
            <Minus size={14} />
          </button>
          <button className="btn sm icon" aria-label="Fit the map" title="Fit the map (drag to move, Ctrl + wheel to zoom)" onClick={fit}>
            <Maximize2 size={14} />
          </button>
        </span>
        <span className="map-legend" aria-hidden>
          <span className="lg-line" /> calls
          <span className="lg-line dashed" /> started without waiting
          <span className="lg-line dotted" /> results back
          <span className="lg-dot" /> in flight
          <span className="lg-diamond" /> failed
          <span className="lg-ring" /> success share (24h)
        </span>
      </div>
      <div
        className="map"
        ref={frame}
        role="group"
        aria-label={`Map of ${deps.length} deployments and ${edges.length} paths${hideQuiet ? ` (${quiet.size} quiet hidden)` : ""}`}
        style={{ height: frameH }}
        onPointerDown={onDown}
        onPointerMove={onMove}
        onPointerUp={() => (drag.current = null)}
        data-focus={focus ? true : undefined}
      >
        <svg width="100%" height={frameH - 18}>
          <g transform={`translate(${view.x} ${view.y}) scale(${view.k})`}>
          <defs>
            <marker id="arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="8" markerHeight="8" markerUnits="userSpaceOnUse" orient="auto">
              <path d="M0 0 8 4 0 8z" className="map-arrow" />
            </marker>
            {nodes.map((n) => (
              <clipPath key={n.key} id={clipId(n.key)}>
                <rect x={n.x + 4} y={n.y} width={n.w - 40} height={n.h} />
              </clipPath>
            ))}
          </defs>
          {edges.map((e) => {
            const a = at.get(e.from);
            const b = at.get(e.to);
            if (!a || !b) return null;
            const { d, mx, my: baseY } = edgePath(a, b);
            const my = a !== b && b.x > a.x + a.w ? baseY + (labelRow.get(e.key) ?? 0) * 16 : baseY;
            const share = e.calls ? e.failed / e.calls : 0;
            const tone = share >= 0.2 ? "err" : e.failed > 0 ? "warn" : undefined;
            const inFlight = flightsOn.get(e.key) ?? [];
            const live = inFlight.filter((f) => !f.endedAt).length;
            const noun = e.kind === "reply" ? "result" : e.kind === "entry" ? "run" : "call";
            const text =
              label === "errors"
                ? e.failed
                  ? `${e.failed} failed · ${Math.round(share * 100)}%`
                  : "no errors"
                : label === "latency"
                  ? e.p95Ms
                    ? `p95 ${duration(e.p95Ms)}`
                    : "—"
                  : plural(e.calls, noun);
            return (
              <g
                key={e.key}
                className="map-edge"
                data-kind={e.kind}
                data-tone={tone}
                data-live={live > 0 || undefined}
                data-dim={focus && !(focus.hit.has(e.from) || focus.hit.has(e.to)) ? true : undefined}
                role="button"
                tabIndex={0}
                aria-label={`${e.from} to ${e.to} (${e.kind}): ${plural(e.calls, noun)}, ${e.failed} failed${live ? `, ${live} in flight` : ""}`}
                onClick={() => onEdge(e)}
                onKeyDown={(ev) => (ev.key === "Enter" || ev.key === " ") && (ev.preventDefault(), onEdge(e))}
              >
                <path className="hit" d={d} />
                <path d={d} markerEnd="url(#arrow)" />
                <text x={mx} y={my - 7} textAnchor="middle">
                  {text}
                  {live > 0 ? ` · ${live} live` : ""}
                </text>
                {inFlight.map((f, i) =>
                  reduced ? (
                    <circle key={f.key} r={4} className="map-flight" data-kind={f.kind} data-failed={f.failed || undefined} cx={mx} cy={my + 8 + i * 9} />
                  ) : f.endedAt ? (
                    // Coming back with the answer (a red diamond if it failed).
                    <rect
                      key={f.key}
                      className="map-flight"
                      data-kind={f.kind}
                      data-failed={f.failed || undefined}
                      x={-4}
                      y={-4}
                      width={8}
                      height={8}
                      rx={f.failed ? 0 : 4}
                      transform={f.failed ? "rotate(45)" : undefined}
                    >
                      <animateMotion dur="1.2s" repeatCount="1" fill="freeze" path={d} keyPoints="1;0" keyTimes="0;1" calcMode="linear" />
                    </rect>
                  ) : (
                    <circle key={f.key} r={4} className="map-flight" data-kind={f.kind}>
                      <animateMotion dur={f.kind === "entry" ? "2.4s" : "1.6s"} begin={`${(i * 0.4) % 1.6}s`} repeatCount="indefinite" path={d} />
                    </circle>
                  ),
                )}
              </g>
            );
          })}
          {nodes.map((n) => {
            if (n.entry) {
              const runs = edges.filter((e) => e.from === n.key).reduce((a, e) => a + e.calls, 0);
              return (
                <g key={n.key} className="map-entry" role="img" aria-label={`${n.entry}: ${plural(runs, "run")} in 24h`}>
                  <rect x={n.x} y={n.y} width={n.w} height={n.h} rx={n.h / 2} />
                  <text x={n.x + n.w / 2} y={n.y + 19} textAnchor="middle" className="map-name">
                    {n.entry}
                  </text>
                  <text x={n.x + n.w / 2} y={n.y + 34} textAnchor="middle" className="map-sub">
                    {plural(runs, "run")} in 24h
                  </text>
                </g>
              );
            }
            const d = n.d!;
            const { x, y } = n;
            const st = deploymentStatus(d);
            const m = metrics.get(n.key);
            const l = live.get(n.key);
            const runs = m?.runs ?? 0;
            const failShare = runs ? (m?.failed ?? 0) / runs : 0;
            const asleep = !l?.running && !(d.ready ?? 0) && !(d.desired ?? 0);
            // Line 2: what it does now. Line 3: what waits for it, or its day.
            const now = d.providerHold
              ? "provider account held"
              : l?.running
                ? [`${l.running} active`, l.approvals ? `${l.approvals} ${l.approvals === 1 ? "needs" : "need"} approval` : "", l.waitingAgent ? `${l.waitingAgent} ${l.waitingAgent === 1 ? "waits" : "wait"} for agents` : ""]
                    .filter(Boolean)
                    .join(" · ")
                : st.label;
            const backlog = [l?.queued ? `${l.queued} queued` : "", l?.scheduled ? `${l.scheduled} scheduled` : ""].filter(Boolean).join(" · ");
            const day = `${compact(runs)} ${runs === 1 ? "run" : "runs"} in 24h${m?.failed ? ` · ${m.failed} failed` : ""}${m?.usage?.costUsd ? ` · ${money(m.usage.costUsd)}` : ""}`;
            const cx = x + W - (big ? 18 : 22);
            const cy = y + (big ? 18 : 22);
            return (
              <g
                key={n.key}
                className="map-node"
                data-tone={d.providerHold ? "err" : st.tone}
                data-live={(l?.running ?? 0) > 0 || undefined}
                data-thinking={(l?.thinking ?? 0) > 0 || undefined}
                data-asleep={asleep || undefined}
                data-held={d.providerHold ? true : undefined}
                data-match={focus?.hit.has(n.key) || undefined}
                data-dim={focus && !focus.near.has(n.key) ? true : undefined}
                role="button"
                tabIndex={0}
                aria-label={`${d.name}: ${now}${backlog ? `, ${backlog}` : ""}`}
                onClick={() => onNode(d)}
                onKeyDown={(ev) => (ev.key === "Enter" || ev.key === " ") && (ev.preventDefault(), onNode(d))}
              >
                <title>{`${d.name}\n${now}${backlog ? `\n${backlog}` : ""}\n${day}`}</title>
                <rect x={x} y={y} width={W} height={H} rx={8} />
                <circle cx={x + 16} cy={y + 20} r={4} className="map-dot" />
                <g clipPath={`url(#${clipId(n.key)})`}>
                  <text x={x + 28} y={y + 24} className="map-name">
                    {cut(d.name, big ? 16 : 20)}
                  </text>
                  {big ? (
                    <text x={x + 16} y={y + 40} className={l?.running ? "map-now" : "map-sub"} data-backlog={!l?.running && backlog ? true : undefined}>
                      {cut(
                        l?.running
                          ? [`${l.running} active`, l.approvals ? `${l.approvals} approval` : "", l.waitingAgent ? `${l.waitingAgent} waiting` : ""].filter(Boolean).join(" · ")
                          : backlog || `${st.label} · ${compact(runs)} in 24h`,
                        24,
                      )}
                    </text>
                  ) : (
                    <>
                      <text x={x + 16} y={y + 44} className="map-now">
                        {cut(now, 30)}
                      </text>
                      <text x={x + 16} y={y + 61} className="map-sub" data-backlog={backlog ? true : undefined}>
                        {cut(backlog || day, 32)}
                      </text>
                    </>
                  )}
                </g>
                {runs > 0 && (
                  <g className="map-ring" aria-hidden>
                    <circle cx={cx} cy={cy} r={big ? 7 : 9} className="ring-track" />
                    <path d={arc(cx, cy, big ? 7 : 9, 0, 1 - failShare)} className="ring-ok" />
                    {failShare > 0 && <path d={arc(cx, cy, big ? 7 : 9, 1 - failShare, 1)} className="ring-err" />}
                  </g>
                )}
              </g>
            );
          })}
          </g>
        </svg>
      </div>
    </div>
  );
}

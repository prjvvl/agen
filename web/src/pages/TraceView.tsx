import {
  ArrowRightLeft,
  Bot,
  ChevronDown,
  ChevronRight,
  CircleDashed,
  MessageSquare,
  RotateCcw,
  ShieldQuestion,
  Sparkles,
  Wrench,
} from "lucide-react";
import { ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { call, num, Run, Span, TranscriptMessage } from "../api";
import { useSession } from "../App";
import { compact, duration, money, ms, tokens } from "../format";
import { useQuery } from "../live";
import { deploymentHref, setQuery } from "../router";
import { Alert, At, Badge, Copy, Empty, Json, runStatus, SkeletonRows, Status, Tabs } from "../ui";
import { MessageView, resultsById, ToolCallBlock, TranscriptView, useTranscript } from "./Transcript";

type Kind = "run" | "model" | "tool" | "agent" | "wait" | "other";

interface Node {
  span: Span;
  kind: Kind;
  depth: number;
  children: Node[];
  start: number;
  end: number;
  running: boolean;
  label: string;
}

const attr = (s: Span, k: string) => s.attributes?.[k];
const str = (v: unknown) => (typeof v === "string" ? v : v === undefined || v === null ? "" : String(v));

function kindOf(s: Span): Kind {
  if (s.name.startsWith("agen.run")) return "run";
  if (s.name === "gen_ai.chat") return "model";
  if (s.name === "agen.tool") return attr(s, "gen_ai.tool.name") === "call_agent" ? "agent" : "tool";
  if (s.name === "agen.permission.check") return "wait";
  return "other";
}

const KIND_ICON: Record<Kind, typeof Bot> = {
  run: Bot,
  model: Sparkles,
  tool: Wrench,
  agent: ArrowRightLeft,
  wait: ShieldQuestion,
  other: CircleDashed,
};

const KIND_LABEL: Record<Kind, string> = {
  run: "Agent run",
  model: "Model call",
  tool: "Tool call",
  agent: "Delegated call",
  wait: "Approval",
  other: "Span",
};

function build(spans: Span[], runs: Map<string, Run>, now: number): Node[] {
  const nodes = new Map<string, Node>();
  for (const s of spans) {
    const run = runs.get(s.runId);
    const kind = kindOf(s);
    const unfinished = s.status === "unfinished";
    const running = unfinished && !!run && !run.endedAt;
    let label = s.name;
    if (kind === "run") label = `${run?.deployment ?? "run"}${s.name.endsWith("resume") ? " (resumed)" : ""}`;
    else if (kind === "model") label = str(attr(s, "gen_ai.request.model")) || "model";
    else if (kind === "tool" || kind === "agent") label = str(attr(s, "gen_ai.tool.name"));
    else if (kind === "wait") label = `approval${attr(s, "agen.permission.decision") ? ": " + str(attr(s, "agen.permission.decision")) : ""}`;
    const start = ms(s.start);
    nodes.set(s.spanId, { span: s, kind, depth: 0, children: [], start, end: running ? now : Math.max(ms(s.end), start), running, label });
  }
  const roots: Node[] = [];
  for (const n of nodes.values()) {
    const p = n.span.parentSpanId ? nodes.get(n.span.parentSpanId) : undefined;
    if (p) p.children.push(n);
    else roots.push(n);
  }
  const fix = (n: Node, depth: number) => {
    n.depth = depth;
    n.children.sort((a, b) => a.start - b.start);
    n.children.forEach((c) => fix(c, depth + 1));
  };
  roots.sort((a, b) => a.start - b.start).forEach((r) => fix(r, 0));
  return roots;
}

function failed(n: Node) {
  return n.span.status === "error" || n.span.status === "denied" || (n.span.status === "unfinished" && !n.running);
}

export function TraceView({ traceId, span }: { traceId: string; span?: string }) {
  const { openAssistant } = useSession();
  const trace = useQuery(() => call<{ spans?: Span[]; runs?: Run[] }>("GetTrace", { traceId }), [traceId], {
    on: (e) => e.kind === "run" && e.traceId === traceId,
  });
  const [tab, setTab] = useState<"timeline" | "transcript">("timeline");
  const [transcriptRun, setTranscriptRun] = useState<string>();
  const [now, setNow] = useState(Date.now());
  const runs = useMemo(() => new Map((trace.data?.runs ?? []).map((r) => [r.id, r])), [trace.data]);
  const live = (trace.data?.runs ?? []).some((r) => !r.endedAt);
  useEffect(() => {
    if (!live) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [live]);
  const roots = useMemo(() => build(trace.data?.spans ?? [], runs, now), [trace.data, runs, now]);

  if (trace.error) return <div className="content"><Alert>{trace.error}</Alert></div>;
  if (trace.loading)
    return (
      <div className="content">
        <SkeletonRows rows={8} cols={3} />
      </div>
    );
  if (!roots.length)
    return (
      <div className="content">
        <Empty title="No spans for this trace">The trace id may be wrong, belong to another namespace, or its data was pruned (see agen hub serve --retention).</Empty>
      </div>
    );

  const all: Node[] = [];
  const walk = (n: Node) => {
    all.push(n);
    n.children.forEach(walk);
  };
  roots.forEach(walk);
  const rootRun = runs.get(roots[0].span.runId) ?? trace.data?.runs?.[0];
  const t0 = Math.min(...all.map((n) => n.start));
  const t1 = Math.max(...all.map((n) => n.end), t0 + 1);
  const usage = (trace.data?.runs ?? []).reduce(
    (a, r) => ({ tokens: a.tokens + tokens(r.usage), cost: a.cost + (r.usage?.costUsd ?? 0) }),
    { tokens: 0, cost: 0 },
  );
  const count = (k: Kind) => all.filter((n) => n.kind === k).length;
  const errors = all.filter(failed).length;
  const status = live ? "running" : rootRun?.status;
  const runList = [...runs.values()].sort((a, b) => ms(a.startedAt) - ms(b.startedAt));
  const shownRun = transcriptRun ?? rootRun?.id;

  return (
    <div className="trace">
      <div className="trace-head">
        <div className="page-head">
          <div className="titles">
            <div className="crumbs">
              <a href="#/runs">Runs</a>
              <span>/</span>
              <Copy text={traceId} display={`trace ${traceId.slice(0, 12)}`} />
            </div>
            <h1 className="truncate" title={rootRun?.input}>
              {rootRun?.input?.split("\n")[0] || "Trace"}
            </h1>
            <div className="facts" data-field="summary">
              <Status spec={runStatus(status)} />
              <span>
                <b>{duration(t1 - t0)}</b>
              </span>
              <span>
                <b>{runs.size}</b> {runs.size === 1 ? "run" : "runs"}
              </span>
              <span>
                <b>{count("model")}</b> model calls
              </span>
              <span>
                <b>{count("tool") + count("agent")}</b> tool calls
              </span>
              <span>
                <b>{compact(usage.tokens)}</b> tokens
              </span>
              {usage.cost > 0 && (
                <span>
                  <b>{money(usage.cost)}</b>
                </span>
              )}
              {errors > 0 && <Badge tone="err">{errors} failed</Badge>}
              <At t={rootRun?.startedAt} />
            </div>
          </div>
          <div className="actions">
            <button className="btn" onClick={() => openAssistant(`Explain trace ${traceId}: what happened, step by step, and why it ended as it did.`)}>
              <MessageSquare size={15} aria-hidden />
              Explain
            </button>
          </div>
        </div>
        <Tabs
          value={tab}
          onChange={setTab}
          tabs={[
            ["timeline", "Timeline"],
            ["transcript", "Transcript"],
          ]}
        />
      </div>
      {tab === "timeline" ? (
        <Timeline roots={roots} runs={runs} t0={t0} t1={t1} selected={span} />
      ) : (
        <div className="trace-transcript">
          {runList.length > 1 && (
            <nav className="run-picker" aria-label="Runs in this trace">
              {runList.map((r) => (
                <button key={r.id} aria-current={r.id === shownRun || undefined} onClick={() => setTranscriptRun(r.id)}>
                  <Status spec={runStatus(r.endedAt ? r.status : "running")}>{r.deployment}</Status>
                  <span className="muted">{r.parentRunId ? "delegated" : "started the trace"}</span>
                </button>
              ))}
            </nav>
          )}
          <div className="trace-transcript-body">{shownRun && <TranscriptView runId={shownRun} live={!runs.get(shownRun)?.endedAt} />}</div>
        </div>
      )}
    </div>
  );
}

const ROW = 30;

function Timeline({ roots, runs, t0, t1, selected }: { roots: Node[]; runs: Map<string, Run>; t0: number; t1: number; selected?: string }) {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const [search, setSearch] = useState("");
  const [onlyErrors, setOnlyErrors] = useState(false);
  const scroller = useRef<HTMLDivElement>(null);
  const [view, setView] = useState({ top: 0, height: 600 });

  const rows = useMemo(() => {
    const q = search.trim().toLowerCase();
    const matches = (n: Node): boolean =>
      (!q || n.label.toLowerCase().includes(q) || n.span.name.includes(q)) && (!onlyErrors || failed(n));
    const out: Node[] = [];
    const keep = (n: Node): boolean => matches(n) || n.children.some(keep);
    const walk = (n: Node) => {
      if ((q || onlyErrors) && !keep(n)) return;
      out.push(n);
      if (!collapsed.has(n.span.spanId) || q || onlyErrors) n.children.forEach(walk);
    };
    roots.forEach(walk);
    return out;
  }, [roots, collapsed, search, onlyErrors]);

  const byId = useMemo(() => {
    const m = new Map<string, Node>();
    const walk = (n: Node) => {
      m.set(n.span.spanId, n);
      n.children.forEach(walk);
    };
    roots.forEach(walk);
    return m;
  }, [roots]);
  const sel = (selected && byId.get(selected)) || roots[0];
  const select = (n: Node) => setQuery({ span: n.span.spanId });

  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const update = () => setView({ top: el.scrollTop, height: el.clientHeight });
    update();
    el.addEventListener("scroll", update, { passive: true });
    const ro = new ResizeObserver(update);
    ro.observe(el);
    return () => {
      el.removeEventListener("scroll", update);
      ro.disconnect();
    };
  }, []);

  const toggle = (id: string) =>
    setCollapsed((c) => {
      const next = new Set(c);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  function onKey(e: React.KeyboardEvent) {
    const i = rows.findIndex((r) => r === sel);
    const move = (j: number) => {
      const n = rows[Math.max(0, Math.min(rows.length - 1, j))];
      if (!n) return;
      select(n);
      const el = scroller.current;
      const y = rows.indexOf(n) * ROW;
      if (el && (y < el.scrollTop || y > el.scrollTop + el.clientHeight - ROW * 2)) el.scrollTop = y - el.clientHeight / 2;
    };
    if (e.key === "ArrowDown") move(i + 1);
    else if (e.key === "ArrowUp") move(i - 1);
    else if (e.key === "ArrowLeft" && sel.children.length && !collapsed.has(sel.span.spanId)) toggle(sel.span.spanId);
    else if (e.key === "ArrowRight" && collapsed.has(sel.span.spanId)) toggle(sel.span.spanId);
    else return;
    e.preventDefault();
  }

  const span = t1 - t0;
  const first = Math.max(0, Math.floor(view.top / ROW) - 10);
  const last = Math.min(rows.length, Math.ceil((view.top + view.height) / ROW) + 10);
  const ticks = [0, 0.25, 0.5, 0.75, 1];

  return (
    <div className="trace-body">
      <div className="waterfall">
        <div className="toolbar wf-tools">
          <input type="search" placeholder="Find a span: tool, model or agent" value={search} onChange={(e) => setSearch(e.target.value)} aria-label="Find span" />
          <label className="check">
            <input type="checkbox" checked={onlyErrors} onChange={(e) => setOnlyErrors(e.target.checked)} /> Only failures
          </label>
          <span className="grow" />
          <span className="muted legend">
            {(["run", "model", "tool", "agent", "wait"] as Kind[]).map((k) => (
              <span key={k}>
                <i data-kind={k} />
                {KIND_LABEL[k]}
              </span>
            ))}
          </span>
        </div>
        <div className="wf-axis" aria-hidden>
          <div />
          <div className="wf-ticks">
            {ticks.map((t) => (
              <span key={t} style={{ left: `${t * 100}%` }}>
                {duration(span * t)}
              </span>
            ))}
          </div>
        </div>
        <div
          className="wf-rows"
          ref={scroller}
          tabIndex={0}
          onKeyDown={onKey}
          role="tree"
          aria-label="Spans"
          aria-activedescendant={sel ? `span-${sel.span.spanId}` : undefined}
        >
          <div style={{ height: rows.length * ROW, position: "relative" }}>
            {rows.slice(first, last).map((n, k) => {
              const i = first + k;
              const Icon = n.kind === "run" && n.span.name.endsWith("resume") ? RotateCcw : KIND_ICON[n.kind];
              const left = ((n.start - t0) / span) * 100;
              const width = Math.max(((n.end - n.start) / span) * 100, 0.4);
              const run = runs.get(n.span.runId);
              const isSel = n === sel;
              return (
                <div
                  key={n.span.spanId}
                  id={`span-${n.span.spanId}`}
                  role="treeitem"
                  aria-selected={isSel}
                  aria-level={n.depth + 1}
                  aria-expanded={n.children.length ? !collapsed.has(n.span.spanId) : undefined}
                  className="wf-row"
                  data-span={n.span.name}
                  data-kind={n.kind}
                  data-failed={failed(n) || undefined}
                  style={{ top: i * ROW }}
                  onClick={() => select(n)}
                >
                  <div className="wf-name" style={{ paddingLeft: 8 + n.depth * 16 }}>
                    {n.children.length ? (
                      <button
                        className="wf-toggle"
                        aria-label={collapsed.has(n.span.spanId) ? "Expand" : "Collapse"}
                        tabIndex={-1}
                        onClick={(e) => {
                          e.stopPropagation();
                          toggle(n.span.spanId);
                        }}
                      >
                        {collapsed.has(n.span.spanId) ? <ChevronRight size={13} /> : <ChevronDown size={13} />}
                      </button>
                    ) : (
                      <span className="wf-toggle" />
                    )}
                    <Icon size={13} className="wf-icon" aria-hidden />
                    <span className="truncate">{n.label}</span>
                    {n.kind === "run" && run && n.depth > 0 && <span className="muted truncate">{run.namespace}</span>}
                  </div>
                  <div className="wf-track">
                    <div className="wf-bar" data-running={n.running || undefined} style={{ left: `${left}%`, width: `${width}%` }} />
                    <span className="wf-dur" style={left > 70 ? { right: `${100 - left + 1}%` } : { left: `calc(${left + width}% + 6px)` }}>
                      {n.running ? "running" : duration(n.end - n.start)}
                    </span>
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      </div>
      <aside className="inspector" aria-label="Span details">
        {sel && <Inspector node={sel} run={runs.get(sel.span.runId)} t0={t0} />}
      </aside>
    </div>
  );
}

function Inspector({ node, run, t0 }: { node: Node; run?: Run; t0: number }) {
  const s = node.span;
  const Icon = KIND_ICON[node.kind];
  const spanStatus = node.running ? "running" : s.status === "ok" ? "succeeded" : s.status === "error" ? "failed" : s.status;
  return (
    <div className="inspector-body">
      <div className="inspector-head">
        <span className="kind-chip" data-kind={node.kind}>
          <Icon size={13} aria-hidden />
          {KIND_LABEL[node.kind]}
        </span>
        <h2 className="truncate" title={node.label}>
          {node.label}
        </h2>
        <div className="facts">
          <Status spec={runStatus(spanStatus)} />
          <span>{node.running ? `for ${duration(node.end - node.start)}` : duration(node.end - node.start)}</span>
          <span>at +{duration(node.start - t0)}</span>
        </div>
      </div>
      {node.kind === "run" && run && <RunDetails run={run} />}
      {node.kind === "model" && <ModelDetails span={s} />}
      {(node.kind === "tool" || node.kind === "agent") && <ToolDetails span={s} />}
      {node.kind === "wait" && (
        <dl className="kv">
          <dt>Decision</dt>
          <dd>{str(attr(s, "agen.permission.decision")) || "waiting"}</dd>
          <dt>Waited</dt>
          <dd>{node.running ? "still waiting" : duration(node.end - node.start)}</dd>
          <dd style={{ gridColumn: "1 / -1" }}>
            <a href="#/inbox">Open the inbox</a>
          </dd>
        </dl>
      )}
      <details className="raw">
        <summary>All attributes</summary>
        <dl className="kv">
          <dt>Span</dt>
          <dd>
            <Copy text={s.spanId} />
          </dd>
          <dt>Run</dt>
          <dd>
            <Copy text={s.runId} />
          </dd>
          <dt>Name</dt>
          <dd className="mono">{s.name}</dd>
          <dt>Status</dt>
          <dd>{s.status}</dd>
        </dl>
        <Json value={s.attributes ?? {}} label="span attributes" />
      </details>
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="section">
      <h3>{title}</h3>
      {children}
    </section>
  );
}

function RunDetails({ run }: { run: Run }) {
  return (
    <>
      <dl className="kv">
        <dt>Deployment</dt>
        <dd>
          <a href={deploymentHref(run.namespace, run.deployment)}>
            {run.namespace}/{run.deployment}
          </a>
        </dd>
        <dt>Run</dt>
        <dd>
          <Copy text={run.id} />
        </dd>
        {run.taskId && (
          <>
            <dt>Task</dt>
            <dd>
              <Copy text={run.taskId} />
            </dd>
          </>
        )}
        <dt>Model turns</dt>
        <dd>{num(run.steps)}</dd>
        <dt>Tokens</dt>
        <dd>
          {compact(run.usage?.inputTokens)} in · {compact(run.usage?.outputTokens)} out
        </dd>
        {(run.usage?.costUsd ?? 0) > 0 && (
          <>
            <dt>Cost</dt>
            <dd>{money(run.usage?.costUsd)}</dd>
          </>
        )}
        {run.labels && Object.keys(run.labels).length > 0 && (
          <>
            <dt>Labels</dt>
            <dd className="toolbar">
              {Object.entries(run.labels).map(([k, v]) => (
                <Badge key={k}>
                  {k}={v}
                </Badge>
              ))}
            </dd>
          </>
        )}
      </dl>
      <Section title="Input">
        <pre className="code">{run.input || "(empty)"}</pre>
      </Section>
      {run.error ? (
        <Section title="Error">
          <Alert>{run.error}</Alert>
        </Section>
      ) : run.output ? (
        <Section title="Output">
          <pre className="code">{run.output}</pre>
        </Section>
      ) : null}
    </>
  );
}

function ModelDetails({ span }: { span: Span }) {
  const t = useTranscript(span.runId, span.status === "unfinished");
  const seq = num(attr(span, "agen.message.seq") as number | undefined);
  const sent = num(attr(span, "agen.request.messages") as number | undefined);
  const messages = t.data?.messages ?? [];
  const reply = messages.find((m) => num(m.seq) === seq);
  const before = seq ? messages.filter((m) => num(m.seq) < seq) : [];
  const input = sent ? before.slice(-sent) : before;
  const results = resultsById(messages);
  const [showInput, setShowInput] = useState(false);
  const [system, setSystem] = useState(false);
  const usageIn = num(attr(span, "gen_ai.usage.input_tokens") as number | undefined);
  const usageOut = num(attr(span, "gen_ai.usage.output_tokens") as number | undefined);
  const cost = Number(attr(span, "agen.usage.cost_usd") ?? 0);
  return (
    <>
      <dl className="kv">
        <dt>Model</dt>
        <dd className="mono">{str(attr(span, "gen_ai.request.model"))}</dd>
        <dt>Provider</dt>
        <dd>{str(attr(span, "gen_ai.provider.name"))}</dd>
        <dt>Tokens</dt>
        <dd>
          {compact(usageIn)} in · {compact(usageOut)} out
        </dd>
        {cost > 0 && (
          <>
            <dt>Cost</dt>
            <dd>{money(cost)}</dd>
          </>
        )}
        <dt>Finished</dt>
        <dd>{str(attr(span, "gen_ai.response.finish_reasons")) || "–"}</dd>
        {attr(span, "error.type") !== undefined && (
          <>
            <dt>Error</dt>
            <dd style={{ color: "var(--err)" }}>{str(attr(span, "error.type"))}</dd>
          </>
        )}
      </dl>
      {t.error && <Alert>{t.error}</Alert>}
      {!seq ? (
        <Alert tone="info">This call is not linked to its messages (it failed, or was recorded by an older version). The run's transcript has the whole conversation.</Alert>
      ) : !t.data ? (
        <SkeletonRows rows={2} cols={1} />
      ) : (
        <>
          <Section title="Received">{reply ? <MessageView m={reply} results={results} /> : <p className="muted">Reply not found.</p>}</Section>
          <section className="section">
            <div className="toolbar">
              <h3>Sent</h3>
              <span className="muted">
                {input.length} {input.length === 1 ? "message" : "messages"}
                {t.data.systemPrompt ? " and the system prompt" : ""}
              </span>
              <span className="grow" />
              <button className="btn sm" onClick={() => setShowInput(!showInput)} aria-expanded={showInput}>
                {showInput ? "Hide" : "Show"}
              </button>
            </div>
            {showInput && (
              <div className="transcript">
                {t.data.systemPrompt && (
                  <button className="btn sm" onClick={() => setSystem(!system)} aria-expanded={system}>
                    {system ? "Hide" : "Show"} system prompt
                  </button>
                )}
                {system && <pre className="code">{t.data.systemPrompt}</pre>}
                {input.map((m) => (
                  <MessageView key={String(m.seq)} m={m} results={results} earlier={m.runId !== span.runId} />
                ))}
              </div>
            )}
          </section>
        </>
      )}
    </>
  );
}

function ToolDetails({ span }: { span: Span }) {
  const t = useTranscript(span.runId, span.status === "unfinished");
  const id = str(attr(span, "gen_ai.tool.call.id"));
  const name = str(attr(span, "gen_ai.tool.name"));
  const messages: TranscriptMessage[] = t.data?.messages ?? [];
  // Providers may reuse call ids across turns: take the last call with this
  // id made before the span ended.
  const end = ms(span.end) || Date.now();
  const callMsg = [...messages]
    .reverse()
    .find((m) => m.runId === span.runId && ms(m.createdAt) <= end + 1000 && m.toolCalls?.some((c) => c.id === id));
  const c = callMsg?.toolCalls?.find((x) => x.id === id);
  const result = messages.find((m) => m.toolCallId === id && num(m.seq) > num(callMsg?.seq));
  const traced = attr(span, "gen_ai.tool.call.arguments");
  return (
    <>
      <dl className="kv">
        <dt>Tool</dt>
        <dd className="mono">{name}</dd>
        <dt>Call</dt>
        <dd>
          <Copy text={id} />
        </dd>
        <dt>Permission</dt>
        <dd>{str(attr(span, "agen.permission.action")) || "–"}</dd>
        {attr(span, "agen.effect") !== undefined && (
          <>
            <dt>Side effect</dt>
            <dd>{str(attr(span, "agen.effect")).replace(/_/g, " ")}</dd>
          </>
        )}
        {attr(span, "agen.tool.result_chars") !== undefined && (
          <>
            <dt>Result size</dt>
            <dd>{compact(num(attr(span, "agen.tool.result_chars") as number))} characters</dd>
          </>
        )}
      </dl>
      {t.error && <Alert>{t.error}</Alert>}
      {c ? (
        <ToolCallBlock call={c} result={result} open />
      ) : traced ? (
        <Section title="Arguments">
          <Json value={traced} />
        </Section>
      ) : t.data ? (
        <p className="muted">The call's arguments and result are not in the run's transcript.</p>
      ) : (
        <SkeletonRows rows={2} cols={1} />
      )}
    </>
  );
}

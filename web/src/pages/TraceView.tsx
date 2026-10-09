import { call, Run, Span } from "../api";
import { usePoll } from "../hooks";

// One trace across agents: spans as a tree with a timeline bar per span.
export function TraceView({ traceId }: { traceId: string }) {
  const { data, error } = usePoll(() => call<{ spans?: Span[]; runs?: Run[] }>("GetTrace", { traceId }), [traceId], 0);
  if (error) return <p className="error">{error}</p>;
  if (!data) return <p>Loading…</p>;
  const spans = data.spans ?? [];
  const runs = Object.fromEntries((data.runs ?? []).map((r) => [r.id, r]));
  const ids = new Set(spans.map((s) => s.spanId));
  const kids: Record<string, Span[]> = {};
  const roots: Span[] = [];
  for (const s of spans) {
    if (s.parentSpanId && ids.has(s.parentSpanId)) (kids[s.parentSpanId] ??= []).push(s);
    else roots.push(s);
  }
  if (spans.length === 0) return <p className="empty">No spans recorded for trace {traceId}.</p>;
  const t = (x?: string) => (x ? new Date(x).getTime() : 0);
  const t0 = Math.min(...spans.map((s) => t(s.start)));
  const t1 = Math.max(...spans.map((s) => t(s.end)), t0 + 1);
  const rows: { s: Span; depth: number }[] = [];
  const walk = (s: Span, depth: number) => {
    rows.push({ s, depth });
    (kids[s.spanId] ?? []).forEach((c) => walk(c, depth + 1));
  };
  roots.forEach((r) => walk(r, 0));
  return (
    <section>
      <h2>
        Trace <span className="mono">{traceId}</span>
      </h2>
      <p className="facts" data-field="summary">
        {data.runs?.length ?? 0} runs, {spans.length} spans, {t1 - t0} ms
      </p>
      <table className="trace">
        <tbody>
          {rows.map(({ s, depth }) => {
            const run = runs[s.runId];
            const isRun = s.name.startsWith("agen.run");
            const tool = s.attributes?.["gen_ai.tool.name"];
            return (
              <tr key={s.spanId} data-span={s.name} data-depth={depth}>
                <td style={{ paddingLeft: `${0.5 + depth * 1.25}rem` }}>
                  <span className={s.status === "ok" ? "" : "error"}>{s.name}</span>
                  {isRun && run && <span className="badge">{`${run.namespace}/${run.deployment}`}</span>}
                  {typeof tool === "string" && <span className="badge">{tool}</span>}
                </td>
                <td className="bar">
                  <div
                    style={{
                      marginLeft: `${((t(s.start) - t0) / (t1 - t0)) * 100}%`,
                      width: `${Math.max(((t(s.end) - t(s.start)) / (t1 - t0)) * 100, 0.5)}%`,
                    }}
                  />
                </td>
                <td className="num">{t(s.end) - t(s.start)} ms</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </section>
  );
}

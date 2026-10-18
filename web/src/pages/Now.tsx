import { Bot, Brain, Hand, Hourglass, Wrench } from "lucide-react";
import { useEffect, useState } from "react";
import { ActiveRun, num } from "../api";
import { compact, duration, money, tokens } from "../format";
import { deploymentHref, traceHref } from "../router";
import { Empty } from "../ui";

/** Ticks every second while mounted, for live elapsed times. */
export function useNow(every = 1000): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), every);
    return () => clearInterval(t);
  }, [every]);
  return now;
}

/** Whole seconds, so the text does not flicker on every tick. */
const since = (t: string | undefined, now: number) => {
  const ms = Math.max(0, now - (Date.parse(t ?? "") || now));
  return ms < 1000 ? "<1 s" : duration(Math.floor(ms / 1000) * 1000);
};

/** What a run is doing, in words, with how long it has been at it. */
export function Doing({ r, now }: { r: ActiveRun; now: number }) {
  const t = since(r.activitySince, now);
  switch (r.activity) {
    case "model":
      return (
        <span className="doing" data-kind="model">
          <Brain size={14} aria-hidden /> Thinking <span className="muted">{t}</span>
        </span>
      );
    case "tool":
      return (
        <span className="doing" data-kind="tool">
          <Wrench size={14} aria-hidden /> <code>{r.detail}</code> <span className="muted">{t}</span>
        </span>
      );
    case "agent":
      return (
        <span className="doing" data-kind="agent">
          <Bot size={14} aria-hidden /> Waiting for <b>{r.detail}</b> <span className="muted">{t}</span>
        </span>
      );
    case "approval":
      return (
        <span className="doing" data-kind="wait">
          <Hand size={14} aria-hidden /> Waiting for{" "}
          <a href="#/inbox" title={r.approvalId}>
            approval
          </a>{" "}
          <span className="muted">{t}</span>
        </span>
      );
    default:
      return (
        <span className="doing" data-kind="idle">
          <Hourglass size={14} aria-hidden /> Between steps <span className="muted">{t}</span>
        </span>
      );
  }
}

/** Runs nested under the run that called them (who waits for whom). */
function tree(runs: ActiveRun[]): { r: ActiveRun; depth: number }[] {
  const ids = new Set(runs.map((r) => r.runId));
  const kids = new Map<string, ActiveRun[]>();
  for (const r of runs) if (r.parentRunId && ids.has(r.parentRunId)) kids.set(r.parentRunId, [...(kids.get(r.parentRunId) ?? []), r]);
  const out: { r: ActiveRun; depth: number }[] = [];
  const walk = (r: ActiveRun, depth: number) => {
    out.push({ r, depth });
    for (const k of kids.get(r.runId) ?? []) walk(k, depth + 1);
  };
  for (const r of runs) if (!r.parentRunId || !ids.has(r.parentRunId)) walk(r, 0);
  return out;
}

/** Every unfinished run and what it is doing now. */
export function NowTable({ runs, flagged }: { runs: ActiveRun[]; flagged: Set<string> }) {
  const now = useNow();
  if (!runs.length) return <Empty title="Nothing is running">Runs appear here the moment they start, with what each is doing.</Empty>;
  return (
    <div className="now-scroll">
    <div className="table-wrap">
      <table className="table now-table">
        <thead>
          <tr>
            <th>Agent</th>
            <th>Doing now</th>
            <th>Task</th>
            <th className="num">Turns</th>
            <th className="num">Tokens</th>
            <th className="num">Cost</th>
            <th className="num">Running for</th>
          </tr>
        </thead>
        <tbody>
          {tree(runs).map(({ r, depth }) => {
            const turns = num(r.turns);
            const max = r.maxTurns ?? 0;
            const near = max > 0 && turns >= 0.8 * max;
            return (
              <tr key={r.runId} data-flagged={flagged.has(r.runId) || undefined}>
                <td>
                  <span className="now-agent" style={{ paddingLeft: depth * 16 }}>
                    {depth > 0 && (
                      <span className="tree-elbow" aria-hidden>
                        └
                      </span>
                    )}
                    <a href={deploymentHref(r.ref.namespace ?? "default", r.ref.name)}>{r.ref.name}</a>
                    {r.source && r.source !== "api" && <span className="muted"> · {r.source}</span>}
                  </span>
                </td>
                <td>
                  <Doing r={r} now={now} />
                </td>
                <td className="now-task">
                  <a className="truncate" href={r.traceId ? traceHref(r.traceId) : undefined} title={r.input}>
                    {r.input?.split("\n")[0] || r.runId}
                  </a>
                </td>
                <td className="num" data-tone={near ? "warn" : undefined} title={max ? `maxTurns ${max}` : undefined}>
                  {turns}
                  {max ? <span className="muted">/{max}</span> : null}
                </td>
                <td className="num">{compact(tokens(r.usage))}</td>
                <td className="num">{r.usage?.costUsd ? money(r.usage.costUsd) : "–"}</td>
                <td className="num">{since(r.startedAt, now)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
    </div>
  );
}

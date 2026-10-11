import { ChevronDown, ChevronRight, CornerDownRight, User, Wrench } from "lucide-react";
import { ReactNode, useState } from "react";
import { call, num, ToolCallMessage, Transcript, TranscriptMessage } from "../api";
import { useQuery } from "../live";
import { Alert, At, Json, SkeletonRows } from "../ui";

const cache = new Map<string, Transcript>();

/** A run's transcript (with its conversation's history), refetched while the run is live. */
export function useTranscript(runId: string | undefined, live: boolean) {
  return useQuery(
    async () => {
      if (!runId) return undefined;
      const hit = cache.get(runId);
      if (hit && !live) return hit;
      const t = await call<Transcript>("GetTranscript", { runId, includeHistory: true });
      if (t.run?.endedAt) cache.set(runId, t);
      return t;
    },
    [runId, live],
    { on: (e) => live && e.kind === "run" && e.id === runId },
  );
}

/** Results of tool calls, by call id. */
export function resultsById(messages: TranscriptMessage[]): Map<string, TranscriptMessage> {
  const m = new Map<string, TranscriptMessage>();
  for (const msg of messages) if (msg.role === "tool" && msg.toolCallId) m.set(msg.toolCallId, msg);
  return m;
}

export function ToolCallBlock({ call: c, result, open: initial = false }: { call: ToolCallMessage; result?: TranscriptMessage; open?: boolean }) {
  const [open, setOpen] = useState(initial);
  const failed = result?.content?.startsWith("error:");
  return (
    <div className="tool-call" data-failed={failed || undefined}>
      <button className="tool-call-head" onClick={() => setOpen(!open)} aria-expanded={open}>
        {open ? <ChevronDown size={14} aria-hidden /> : <ChevronRight size={14} aria-hidden />}
        <Wrench size={13} aria-hidden />
        <span className="mono">{c.name}</span>
        <span className="muted truncate">{argsPreview(c.arguments)}</span>
        {!result && <span className="badge">no result</span>}
        {failed && <span className="badge" data-tone="err">error</span>}
      </button>
      {open && (
        <div className="tool-call-body">
          <div className="label">Arguments</div>
          <Json value={c.arguments ?? {}} label={`arguments of ${c.name}`} />
          {result && (
            <>
              <div className="label">Result</div>
              <Json value={result.content ?? ""} label={`result of ${c.name}`} />
            </>
          )}
        </div>
      )}
    </div>
  );
}

function argsPreview(a: unknown): string {
  if (a === undefined || a === null) return "";
  const s = typeof a === "string" ? a : JSON.stringify(a);
  return s.length > 90 ? s.slice(0, 90) + "…" : s;
}

export function MessageView({ m, results, earlier }: { m: TranscriptMessage; results: Map<string, TranscriptMessage>; earlier?: boolean }) {
  if (m.role === "tool") return null; // shown under the call it answers
  const who: Record<string, ReactNode> = {
    user: (
      <>
        <User size={13} aria-hidden /> Input
      </>
    ),
    assistant: (
      <>
        <CornerDownRight size={13} aria-hidden /> Model
      </>
    ),
  };
  return (
    <div className="msg" data-role={m.role} data-earlier={earlier || undefined} data-seq={num(m.seq)}>
      <div className="msg-head">
        {who[m.role] ?? m.role}
        {earlier && <span className="badge">earlier run</span>}
        <span className="grow" />
        <At t={m.createdAt} />
      </div>
      {m.content && <div className="msg-text">{m.content}</div>}
      {(m.toolCalls ?? []).map((c) => (
        <ToolCallBlock key={c.id} call={c} result={results.get(c.id)} />
      ))}
    </div>
  );
}

/** A run's conversation as it happened, earlier runs folded away. */
export function TranscriptView({ runId, live }: { runId: string; live: boolean }) {
  const t = useTranscript(runId, live);
  const [history, setHistory] = useState(false);
  const [system, setSystem] = useState(false);
  if (t.error) return <Alert>{t.error}</Alert>;
  if (!t.data) return <SkeletonRows rows={4} cols={1} />;
  const all = t.data.messages ?? [];
  const earlier = all.filter((m) => m.runId !== runId);
  const shown = history ? all : all.filter((m) => m.runId === runId);
  const results = resultsById(all);
  return (
    <div className="transcript">
      <div className="toolbar">
        {t.data.systemPrompt && (
          <button className="btn sm" onClick={() => setSystem(!system)} aria-expanded={system}>
            {system ? "Hide" : "Show"} system prompt
          </button>
        )}
        {earlier.length > 0 && (
          <button className="btn sm" onClick={() => setHistory(!history)} aria-expanded={history}>
            {history ? "Hide" : "Show"} {earlier.length} earlier messages
          </button>
        )}
      </div>
      {system && <pre className="code">{t.data.systemPrompt}</pre>}
      {shown.length === 0 ? (
        <p className="muted">No messages recorded yet.</p>
      ) : (
        shown.map((m) => <MessageView key={String(m.seq)} m={m} results={results} earlier={m.runId !== runId} />)
      )}
      {t.data.run?.error && <Alert>{t.data.run.error}</Alert>}
    </div>
  );
}

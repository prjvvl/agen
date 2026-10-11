import { ExternalLink, History, Loader2, Plus, Send, Trash2, Wrench, X } from "lucide-react";
import { PointerEvent as ReactPointerEvent, ReactNode, useEffect, useRef, useState } from "react";
import { call, Deployment, Task } from "../api";
import { useSession } from "../App";
import { ago } from "../format";
import { useQuery } from "../live";
import { traceHref } from "../router";
import { Alert } from "../ui";
import { useTranscript } from "./Transcript";

const NAME = "assistant";

interface Turn {
  role: "user" | "assistant";
  text: string;
  taskId?: string;
  traceId?: string;
  runId?: string;
  state?: "running" | "done" | "failed" | "approval";
}

/** A past chat, newest first in the history. */
interface ChatMeta {
  id: string;
  title: string;
  updated: number;
}

const MAX_CHATS = 30;
const MIN_WIDTH = 360;
const maxWidth = () => Math.max(MIN_WIDTH, Math.min(1200, innerWidth - 80));

// Chats are kept in this browser, per signed-in principal; the assistant
// itself remembers each one by its conversation key.
function load<T>(key: string, fallback: T): T {
  try {
    const v = localStorage.getItem(key);
    return v ? (JSON.parse(v) as T) : fallback;
  } catch {
    return fallback;
  }
}

function save(key: string, value: unknown) {
  try {
    if (value === undefined) localStorage.removeItem(key);
    else localStorage.setItem(key, JSON.stringify(value));
  } catch {
    /* storage unavailable: the chat lasts while the panel is open */
  }
}

/** The console assistant: an agent deployment that works through the Hub's MCP tools with the user's permissions. */
export function Assistant({ prompt, onClose }: { prompt?: string; onClose: () => void }) {
  const { me, can } = useSession();
  const deps = useQuery(() => call<{ deployments?: Deployment[] }>("ListDeployments", {}), [], { on: ["deployment"] });
  const dep = (deps.data?.deployments ?? []).find((d) => d.name === NAME);
  const key = `agen.chat.${me?.id ?? "user"}`;
  const [chats, setChats] = useState<ChatMeta[]>(() => load(`${key}.list`, []));
  const [chat, setChat] = useState<string>(() => load(`${key}.current`, crypto.randomUUID()));
  const [turns, setTurns] = useState<Turn[]>(() => load(`${key}.${chat}`, []));
  // The turns as loaded, which opening a chat does not make recent.
  const loaded = useRef(turns);
  const [history, setHistory] = useState(false);
  const [width, setWidth] = useState(() => Math.min(load("agen.assistant.width", 440), maxWidth()));
  const [input, setInput] = useState(prompt ?? "");
  const [error, setError] = useState("");
  const list = useRef<HTMLDivElement>(null);
  const sent = useRef(false);
  const following = useRef(new Set<string>());
  const alive = useRef(true);
  // Bumped when the open chat changes, so answers being followed for the
  // previous one stop updating this one.
  const opened = useRef(0);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      following.current.clear();
    };
  }, []);
  useEffect(() => {
    // Pick up answers still being worked on when the chat was left.
    for (const t of turns) if (t.taskId && (t.state === "running" || t.state === "approval")) follow(t.taskId);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chat]);

  useEffect(() => save(`${key}.current`, chat), [key, chat]);
  useEffect(() => {
    if (!turns.length || turns === loaded.current) return;
    save(`${key}.${chat}`, turns);
    const title = turns.find((t) => t.role === "user")?.text.slice(0, 80) ?? "";
    setChats((all) => {
      const next = [{ id: chat, title, updated: Date.now() }, ...all.filter((c) => c.id !== chat)];
      for (const old of next.slice(MAX_CHATS)) save(`${key}.${old.id}`, undefined);
      return next.slice(0, MAX_CHATS);
    });
  }, [key, chat, turns]);
  useEffect(() => save(`${key}.list`, chats), [key, chats]);
  useEffect(() => save("agen.assistant.width", width), [width]);
  useEffect(() => {
    list.current?.scrollTo({ top: list.current.scrollHeight });
  }, [turns]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    addEventListener("keydown", onKey);
    return () => removeEventListener("keydown", onKey);
  }, [onClose]);
  useEffect(() => {
    if (prompt && dep && !sent.current) {
      sent.current = true;
      send(prompt);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [prompt, dep]);

  const busy = turns.some((t) => t.state === "running");

  async function send(text: string) {
    if (!dep || !text.trim() || busy) return;
    setError("");
    setInput("");
    const user: Turn = { role: "user", text };
    setTurns((t) => [...t, user]);
    try {
      const r = await call<{ task: Task }>("SubmitTask", {
        ref: { namespace: dep.namespace, name: dep.name },
        input: text,
        conversationKey: `console:${me?.id ?? "user"}:${chat}`,
        labels: { source: "console" },
      });
      setTurns((t) => [...t, { role: "assistant", text: "", taskId: r.task.id, state: "running" }]);
      follow(r.task.id);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  async function follow(id: string) {
    if (following.current.has(id)) return;
    following.current.add(id);
    const mine = opened.current;
    while (alive.current && mine === opened.current) {
      let t: Task;
      try {
        t = (await call<{ task: Task }>("GetTask", { id, waitSeconds: 25 })).task;
        if (mine !== opened.current) break;
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        break;
      }
      const s = t.state.replace("TASK_STATE_", "").toLowerCase();
      const done = s === "succeeded" || s === "failed" || s === "cancelled";
      setTurns((all) =>
        all.map((x) =>
          x.taskId === id
            ? {
                ...x,
                traceId: t.traceId,
                runId: t.runId,
                text: done ? t.output || t.error || "" : x.text,
                state: done ? (s === "succeeded" ? "done" : "failed") : t.pendingApprovalId ? "approval" : "running",
              }
            : x,
        ),
      );
      if (done) break;
    }
    following.current.delete(id);
  }

  function open(id: string) {
    opened.current++;
    following.current.clear();
    setChat(id);
    loaded.current = load(`${key}.${id}`, []);
    setTurns(loaded.current);
    setHistory(false);
    setError("");
  }

  function forget(id: string) {
    save(`${key}.${id}`, undefined);
    setChats((all) => all.filter((c) => c.id !== id));
    if (id === chat) open(crypto.randomUUID());
  }

  // Drag the left edge to resize; arrow keys do the same from the keyboard.
  function onResizeStart(e: ReactPointerEvent<HTMLDivElement>) {
    e.preventDefault();
    const target = e.currentTarget;
    target.setPointerCapture(e.pointerId);
    const move = (m: PointerEvent) => setWidth(Math.round(Math.min(maxWidth(), Math.max(MIN_WIDTH, innerWidth - m.clientX))));
    const up = () => {
      target.removeEventListener("pointermove", move);
      target.removeEventListener("pointerup", up);
    };
    target.addEventListener("pointermove", move);
    target.addEventListener("pointerup", up);
  }

  let body: ReactNode;
  if (deps.loading) body = <div className="skeleton" style={{ height: 60 }} />;
  else if (!dep)
    body = (
      <div className="section">
        <p>
          The assistant is an agent like any other: it answers questions about your fleet and makes changes you confirm, using the Hub's
          tools with <b>your</b> permissions. It is not set up yet.
        </p>
        {can("admin") ? (
          <a className="btn primary" href="#/templates/assistant" onClick={onClose}>
            Set up the assistant
          </a>
        ) : (
          <Alert tone="info">An admin can set it up from Templates → Console assistant.</Alert>
        )}
      </div>
    );
  else if (history)
    body = chats.length ? (
      <ul className="chat-history" aria-label="Past chats">
        {chats.map((c) => (
          <li key={c.id} aria-current={c.id === chat || undefined}>
            <button className="chat-history-open" onClick={() => open(c.id)}>
              <span className="truncate">{c.title || "(untitled)"}</span>
              <span className="muted">{ago(new Date(c.updated).toISOString())}</span>
            </button>
            <button className="btn ghost icon sm" onClick={() => forget(c.id)} aria-label={`Delete chat: ${c.title}`}>
              <Trash2 size={13} aria-hidden />
            </button>
          </li>
        ))}
      </ul>
    ) : (
      <p className="muted">No past chats in this browser yet.</p>
    );
  else
    body = (
      <>
        <div className="chat" ref={list} aria-live="polite">
          {turns.length === 0 && (
            <div className="chat-hints">
              <p className="muted">Ask about your fleet, for example:</p>
              {["What failed in the last hour, and why?", "Which deployment spent the most today?", "Scale researcher to 2 instances."].map((q) => (
                <button key={q} className="btn ghost sm" onClick={() => send(q)}>
                  {q}
                </button>
              ))}
            </div>
          )}
          {turns.map((t, i) => (
            <ChatTurn key={i} t={t} />
          ))}
        </div>
        {error && <Alert>{error}</Alert>}
        <form
          className="chat-input"
          onSubmit={(e) => {
            e.preventDefault();
            send(input);
          }}
        >
          <textarea
            rows={2}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                send(input);
              }
            }}
            placeholder={busy ? "Working…" : "Ask or tell the assistant…"}
            aria-label="Message the assistant"
            autoFocus
          />
          <button className="btn primary icon" type="submit" disabled={busy || !input.trim()} aria-label="Send">
            <Send size={15} />
          </button>
        </form>
      </>
    );

  return (
    <aside className="assistant-panel" aria-label="Assistant" style={{ width: `min(${width}px, 100vw)` }}>
      <div
        className="panel-resize"
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize the assistant"
        aria-valuemin={MIN_WIDTH}
        aria-valuemax={maxWidth()}
        aria-valuenow={width}
        tabIndex={0}
        onPointerDown={onResizeStart}
        onDoubleClick={() => setWidth(440)}
        onKeyDown={(e) => {
          const step = e.key === "ArrowLeft" ? 40 : e.key === "ArrowRight" ? -40 : 0;
          if (step) {
            e.preventDefault();
            setWidth((w) => Math.min(maxWidth(), Math.max(MIN_WIDTH, w + step)));
          }
        }}
      />
      <div className="panel-head">
        <h2>{history ? "Past chats" : "Assistant"}</h2>
        {dep && (
          <button className="btn ghost sm" onClick={() => setHistory((h) => !h)} aria-pressed={history}>
            <History size={13} aria-hidden />
            History
          </button>
        )}
        {dep && (
          <button className="btn ghost sm" onClick={() => open(crypto.randomUUID())} disabled={!history && !turns.length}>
            <Plus size={13} aria-hidden />
            New chat
          </button>
        )}
        <button className="btn ghost icon" onClick={onClose} aria-label="Close assistant">
          <X size={16} />
        </button>
      </div>
      <div className="assistant-body">{body}</div>
    </aside>
  );
}

function ChatTurn({ t }: { t: Turn }) {
  if (t.role === "user") return <div className="turn user">{t.text}</div>;
  return (
    <div className="turn assistant" data-state={t.state}>
      {t.state === "running" || t.state === "approval" ? <Progress runId={t.runId} approval={t.state === "approval"} /> : <Markdown text={t.text} />}
      {t.traceId && (
        <a className="turn-trace" href={traceHref(t.traceId)}>
          <ExternalLink size={12} aria-hidden /> What it did
        </a>
      )}
    </div>
  );
}

/** The tool calls of the assistant's run so far. */
function Progress({ runId, approval }: { runId?: string; approval: boolean }) {
  const t = useTranscript(runId, true);
  const calls = (t.data?.messages ?? []).filter((m) => m.runId === runId).flatMap((m) => m.toolCalls ?? []);
  return (
    <div className="progress">
      <span className="status" data-tone="info" data-active>
        <Loader2 size={14} aria-hidden /> {approval ? "Waiting for an approval" : "Working"}
      </span>
      {calls.map((c) => (
        <span key={c.id} className="muted toolbar">
          <Wrench size={12} aria-hidden /> {c.name.replace(/^agen[._]/, "").replace(/_/g, " ")}
        </span>
      ))}
    </div>
  );
}

/** A small, safe Markdown subset: paragraphs, headings, lists, code, tables and emphasis. */
export function Markdown({ text }: { text: string }) {
  const out: ReactNode[] = [];
  const lines = text.split("\n");
  let i = 0;
  const inline = (s: string): ReactNode[] =>
    s.split(/(`[^`]+`|\*\*[^*]+\*\*|\*[^*\s][^*]*\*)/g).map((p, k) =>
      p.startsWith("`") && p.endsWith("`") && p.length > 1 ? (
        <code key={k}>{p.slice(1, -1)}</code>
      ) : p.startsWith("**") && p.endsWith("**") && p.length > 3 ? (
        <b key={k}>{inline(p.slice(2, -2))}</b>
      ) : p.startsWith("*") && p.endsWith("*") && p.length > 2 ? (
        <i key={k}>{inline(p.slice(1, -1))}</i>
      ) : (
        p
      ),
    );
  while (i < lines.length) {
    const line = lines[i];
    if (line.startsWith("```")) {
      const code: string[] = [];
      i++;
      while (i < lines.length && !lines[i].startsWith("```")) code.push(lines[i++]);
      i++;
      out.push(
        <pre key={out.length} className="code">
          {code.join("\n")}
        </pre>,
      );
    } else if (/^\s*\|.*\|\s*$/.test(line)) {
      const rows: string[][] = [];
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) {
        if (!/^\s*\|[\s:|-]+\|\s*$/.test(lines[i])) rows.push(lines[i].trim().slice(1, -1).split("|").map((c) => c.trim()));
        i++;
      }
      const [head, ...rest] = rows;
      out.push(
        <div key={out.length} className="table-wrap">
          <table>
            <thead>
              <tr>
                {head?.map((c, k) => (
                  <th key={k}>{inline(c)}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rest.map((r, k) => (
                <tr key={k}>
                  {r.map((c, j) => (
                    <td key={j}>{inline(c)}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>,
      );
    } else if (/^\s*([-*]|\d+\.)\s/.test(line)) {
      const ordered = /^\s*\d+\./.test(line);
      const items: string[] = [];
      while (i < lines.length && /^\s*([-*]|\d+\.)\s/.test(lines[i])) items.push(lines[i++].replace(/^\s*([-*]|\d+\.)\s/, ""));
      const L = ordered ? "ol" : "ul";
      out.push(
        <L key={out.length}>
          {items.map((it, k) => (
            <li key={k}>{inline(it)}</li>
          ))}
        </L>,
      );
    } else if (/^#{1,6}\s/.test(line)) {
      out.push(<p key={out.length}><b>{inline(line.replace(/^#+\s/, ""))}</b></p>);
      i++;
    } else if (line.trim() === "") {
      i++;
    } else {
      const para: string[] = [];
      while (i < lines.length && lines[i].trim() !== "" && !/^(```|\s*\||\s*([-*]|\d+\.)\s|#{1,6}\s)/.test(lines[i])) para.push(lines[i++]);
      out.push(<p key={out.length}>{inline(para.join(" "))}</p>);
    }
  }
  return <div className="md">{out}</div>;
}

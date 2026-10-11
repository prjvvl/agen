import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { token } from "./api";

// The Hub's change feed (GET /api/v1/events, server-sent events). Views
// refetch what they show when a matching change arrives, so nothing polls
// every few seconds. Read with fetch, not EventSource, to send the token in
// a header instead of the URL.

export type Kind = "task" | "run" | "approval" | "instance" | "deployment";

export interface LiveEvent {
  kind: Kind | "resync";
  id: string;
  namespace: string;
  deployment: string;
  state?: string;
  traceId?: string;
}

type State = "connecting" | "live" | "offline";

const listeners = new Set<(e: LiveEvent) => void>();
const stateListeners = new Set<() => void>();
let state: State = "connecting";
let controller: AbortController | undefined;
let retry = 0;

function setState(s: State) {
  if (s === state) return;
  state = s;
  stateListeners.forEach((l) => l());
}

function emit(e: LiveEvent) {
  listeners.forEach((l) => l(e));
}

async function connect() {
  controller?.abort();
  const mine = new AbortController();
  controller = mine;
  setState("connecting");
  try {
    const r = await fetch("/api/v1/events", { headers: { Authorization: `Bearer ${token()}` }, signal: mine.signal });
    if (!r.ok || !r.body) throw new Error(String(r.status));
    const reader = r.body.pipeThrough(new TextDecoderStream()).getReader();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += value;
      let cut: number;
      while ((cut = buf.indexOf("\n\n")) >= 0) {
        const block = buf.slice(0, cut);
        buf = buf.slice(cut + 2);
        let event = "message";
        let data = "";
        for (const line of block.split("\n")) {
          if (line.startsWith("event: ")) event = line.slice(7);
          else if (line.startsWith("data: ")) data += line.slice(6);
        }
        if (event === "ready") {
          retry = 0;
          setState("live");
          // Changes may have been missed while disconnected.
          emit({ kind: "resync", id: "", namespace: "", deployment: "" });
        } else if (event === "change" && data) {
          try {
            emit(JSON.parse(data));
          } catch {
            /* ignore a malformed event */
          }
        }
      }
    }
  } catch {
    if (mine.signal.aborted) return;
  }
  if (mine.signal.aborted || !listeners.size) return;
  setState("offline");
  retry = Math.min(retry + 1, 6);
  setTimeout(() => controller === mine && listeners.size && connect(), 500 * 2 ** retry);
}

export function subscribe(fn: (e: LiveEvent) => void): () => void {
  listeners.add(fn);
  if (listeners.size === 1) connect();
  return () => {
    listeners.delete(fn);
    if (!listeners.size) {
      controller?.abort();
      controller = undefined;
      setState("connecting");
    }
  };
}

export function useLiveState(): State {
  return useSyncExternalStore(
    (l) => {
      stateListeners.add(l);
      return () => stateListeners.delete(l);
    },
    () => state,
  );
}

export function useLiveEvents(fn: (e: LiveEvent) => void) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => subscribe((e) => ref.current(e)), []);
}

export interface QueryOptions {
  /** Refetch on these changes (or when the filter says so); resyncs always refetch. */
  on?: Kind[] | ((e: LiveEvent) => boolean);
  /** Fallback refresh while the tab is visible, in ms (0 = none). */
  every?: number;
}

/**
 * useQuery loads data, then refetches when a matching change arrives
 * (coalesced), on an optional timer and when the tab becomes visible.
 * Data stays on screen while refetching; requests never overlap, and a
 * response for old deps is dropped.
 */
export function useQuery<T>(load: () => Promise<T>, deps: unknown[], opts: QueryOptions = {}) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const loadRef = useRef(load);
  loadRef.current = load;
  const optsRef = useRef(opts);
  optsRef.current = opts;
  const gen = useRef(0);
  const busy = useRef(false);
  const again = useRef(false);

  const reload = useCallback(async () => {
    if (busy.current) {
      again.current = true;
      return;
    }
    const mine = gen.current;
    busy.current = true;
    try {
      const d = await loadRef.current();
      if (mine === gen.current) {
        setData(d);
        setError("");
      }
    } catch (e) {
      if (mine === gen.current) setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (mine === gen.current) setLoading(false);
      busy.current = false;
      if (again.current) {
        again.current = false;
        reload();
      }
    }
  }, []);

  useEffect(() => {
    gen.current++;
    busy.current = false;
    again.current = false;
    setData(undefined);
    setError("");
    setLoading(true);
    reload();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const soon = () => {
      clearTimeout(timer);
      timer = setTimeout(reload, 250);
    };
    const unsubscribe = subscribe((e) => {
      const on = optsRef.current.on;
      if (e.kind === "resync" || (Array.isArray(on) ? on.includes(e.kind as Kind) : on?.(e))) soon();
    });
    const every = optsRef.current.every ?? 0;
    const interval = every ? setInterval(() => !document.hidden && reload(), every) : undefined;
    const onVisible = () => !document.hidden && reload();
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearTimeout(timer);
      clearInterval(interval);
      unsubscribe();
      document.removeEventListener("visibilitychange", onVisible);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return { data, error, loading, reload };
}

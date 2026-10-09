import { useCallback, useEffect, useRef, useState } from "react";

// usePoll loads data now and every intervalMs (0 = once) while the tab is
// visible; reload() refreshes immediately (after an action). Requests never
// overlap, and a response is dropped if the deps changed since it started
// (no data from the previous page under the next page's header).
export function usePoll<T>(load: () => Promise<T>, deps: unknown[], intervalMs = 3000) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<string>("");
  const loadRef = useRef(load);
  loadRef.current = load;
  const gen = useRef(0);
  const busy = useRef(false);
  const reload = useCallback(async () => {
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
      busy.current = false;
    }
  }, []);
  useEffect(() => {
    gen.current++;
    busy.current = false;
    setData(undefined);
    setError("");
    reload();
    if (!intervalMs) return;
    const t = setInterval(() => {
      if (!busy.current && !document.hidden) reload();
    }, intervalMs);
    const onVisible = () => !document.hidden && reload();
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearInterval(t);
      document.removeEventListener("visibilitychange", onVisible);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return { data, error, reload };
}

export function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

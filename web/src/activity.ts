import { useEffect, useMemo, useState } from "react";
import { Activity, call } from "./api";
import { useLiveEvents, useQuery } from "./live";

/** What is running now (GetActivity), refetched as runs and steps change. */
export function useActivity() {
  return useQuery(() => call<Activity>("GetActivity", {}), [], { on: ["run", "step", "task", "approval"], every: 15_000 });
}

/** A call from one agent to another that is in flight (or just ended). */
export interface Flight {
  key: string;
  from: string;
  to: string;
  since: number;
  /** Set when it ended; the flight is shown briefly returning, then dropped. */
  endedAt?: number;
  failed?: boolean;
}

const RETURN_MS = 1500;

const depKeyOf = (ns: string | undefined, name: string) => `${ns || "default"}/${name}`;

/**
 * Calls between agents in flight: from GetActivity (runs waiting for an
 * agent) plus step events between refreshes (a delegation starting or
 * ending), so the map moves the moment a call starts.
 */
export function useFlights(activity?: Activity): Flight[] {
  const [steps, setSteps] = useState<Map<string, Flight>>(new Map());
  const [, tick] = useState(0);
  useLiveEvents((e) => {
    if (e.kind !== "step" || e.name !== "agen.tool" || !e.target) return;
    const key = `${e.runId}>${e.target}`;
    setSteps((m) => {
      const next = new Map(m);
      if (e.state === "unfinished") next.set(key, { key, from: depKeyOf(e.namespace, e.deployment), to: depKeyOf(e.namespace, e.target!), since: e.startMs ?? Date.now() });
      else {
        const f = next.get(key) ?? { key, from: depKeyOf(e.namespace, e.deployment), to: depKeyOf(e.namespace, e.target!), since: e.startMs ?? Date.now() };
        next.set(key, { ...f, endedAt: Date.now(), failed: e.state !== "ok" });
      }
      return next;
    });
  });
  // Drop returned flights.
  useEffect(() => {
    const t = setInterval(() => {
      setSteps((m) => {
        const now = Date.now();
        let changed = false;
        const next = new Map(m);
        for (const [k, f] of m) {
          if (f.endedAt && now - f.endedAt > RETURN_MS) {
            next.delete(k);
            changed = true;
          }
        }
        return changed ? next : m;
      });
      tick((n) => n + 1);
    }, 500);
    return () => clearInterval(t);
  }, []);
  return useMemo(() => {
    const out = new Map<string, Flight>();
    for (const r of activity?.runs ?? []) {
      if (r.activity !== "agent" || !r.detail) continue;
      const key = `${r.runId}>${r.detail}`;
      out.set(key, { key, from: depKeyOf(r.ref.namespace, r.ref.name), to: depKeyOf(r.ref.namespace, r.detail), since: Date.parse(r.activitySince ?? "") || Date.now() });
    }
    for (const [k, f] of steps) {
      // A step event that ended overrides the (older) activity snapshot.
      if (f.endedAt || !out.has(k)) out.set(k, f);
    }
    return [...out.values()];
  }, [activity, steps]);
}

import { useEffect, useMemo, useState } from "react";
import { Activity, call } from "./api";
import { useLiveEvents, useQuery } from "./live";

/** What is running now (GetActivity), refetched as runs and steps change. */
export function useActivity() {
  return useQuery(() => call<Activity>("GetActivity", {}), [], { on: ["run", "step", "task", "approval"], every: 15_000 });
}

/**
 * Work in flight between two places on the map:
 * - call: an agent called another and waits for the answer;
 * - async: an agent started another (start_agent) and went on;
 * - reply: the result of async work going back to the agent that started it;
 * - entry: work from outside the fleet (API or CLI, a schedule, a webhook,
 *   an outside A2A caller) being run.
 */
export type FlightKind = "call" | "async" | "reply" | "entry";

export interface Flight {
  key: string;
  /** A deployment key (ns/name) or an entry node ("entry:api"). */
  from: string;
  to: string;
  kind: FlightKind;
  since: number;
  /** Set when it ended; the flight is shown briefly returning, then dropped. */
  endedAt?: number;
  failed?: boolean;
}

const RETURN_MS = 1500;

const depKeyOf = (ns: string | undefined, name: string) => `${ns || "default"}/${name}`;

/** The entry node a run's work came from, by its task source. */
export function entryOf(source?: string): string {
  const kind = (source ?? "").split(":")[0];
  if (kind === "api" || kind === "cron" || kind === "webhook") return `entry:${kind}`;
  return "entry:a2a";
}

/**
 * Everything in flight now: from GetActivity (each run, from whatever
 * started it), plus step events between refreshes so a call shows the moment
 * it starts and comes back when it ends.
 */
export function useFlights(activity?: Activity): Flight[] {
  const [steps, setSteps] = useState<Map<string, Flight>>(new Map());
  const [, tick] = useState(0);
  useLiveEvents((e) => {
    if (e.kind !== "step" || e.name !== "agen.tool" || !e.target) return;
    const kind: FlightKind = e.tool === "start_agent" ? "async" : "call";
    const key = `${e.runId}>${e.target}`;
    const base = { key, from: depKeyOf(e.namespace, e.deployment), to: depKeyOf(e.namespace, e.target!), kind, since: e.startMs ?? Date.now() };
    setSteps((m) => {
      const next = new Map(m);
      if (e.state === "unfinished") next.set(key, base);
      else next.set(key, { ...(next.get(key) ?? base), endedAt: Date.now(), failed: e.state !== "ok" });
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
    const runs = activity?.runs ?? [];
    const running = new Set(runs.map((r) => r.runId));
    for (const r of runs) {
      const to = depKeyOf(r.ref.namespace, r.ref.name);
      const since = Date.parse(r.startedAt ?? "") || Date.now();
      if (r.caller) {
        // Started by another agent: a call while that run waits, async otherwise.
        const from = depKeyOf(r.caller.namespace, r.caller.name);
        const kind: FlightKind = r.parentRunId && running.has(r.parentRunId) ? "call" : "async";
        out.set(`${r.parentRunId}>${r.ref.name}`, { key: `run:${r.runId}`, from, to, kind, since });
      } else if (r.source?.startsWith("reply:")) {
        const from = depKeyOf(r.ref.namespace, r.source.slice(6));
        out.set(`run:${r.runId}`, { key: `run:${r.runId}`, from, to, kind: "reply", since });
      } else {
        out.set(`run:${r.runId}`, { key: `run:${r.runId}`, from: entryOf(r.source), to, kind: "entry", since });
      }
    }
    for (const [k, f] of steps) {
      // A step event that ended overrides the (older) activity snapshot.
      if (f.endedAt || !out.has(k)) out.set(k, f);
    }
    return [...out.values()];
  }, [activity, steps]);
}

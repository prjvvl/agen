import { useEffect, useState } from "react";

// Hash routes, so the Hub serves one page and `agen ui` sign-in links
// (#token=...) keep working:
//   #/                                 overview
//   #/inbox                            approvals and failures
//   #/deployments[/<ns>/<name>[/tab]]  deployments
//   #/runs?status=failed               runs and traces
//   #/trace/<traceId>[?span=<id>]      one trace
//   #/costs, #/templates, #/fleet, #/settings[/tab]
//   #/new, #/edit/<ns>/<name>          bundle editor

export interface Route {
  path: string[];
  query: URLSearchParams;
}

function decode(s: string) {
  try {
    return decodeURIComponent(s);
  } catch {
    return s; // malformed escape: keep it as typed
  }
}

export function parseRoute(hash = location.hash): Route {
  const raw = hash.replace(/^#\/?/, "");
  const [p, q = ""] = raw.split("?");
  return { path: p ? p.split("/").map(decode) : [], query: new URLSearchParams(q) };
}

export function useRoute(): Route {
  const [route, setRoute] = useState(parseRoute);
  useEffect(() => {
    const on = () => setRoute(parseRoute());
    addEventListener("hashchange", on);
    return () => removeEventListener("hashchange", on);
  }, []);
  return route;
}

export function href(path: (string | undefined)[], query?: Record<string, string | undefined>): string {
  const p = path.filter((x) => x !== undefined && x !== "").map((x) => encodeURIComponent(x!)).join("/");
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) if (v) q.set(k, v);
  const qs = q.toString();
  return `#/${p}${qs ? "?" + qs : ""}`;
}

export function go(path: (string | undefined)[], query?: Record<string, string | undefined>) {
  location.hash = href(path, query);
}

/** Replaces the query of the current route without a history entry. */
export function setQuery(query: Record<string, string | undefined>) {
  const { path, query: cur } = parseRoute();
  const next: Record<string, string | undefined> = Object.fromEntries(cur.entries());
  Object.assign(next, query);
  history.replaceState(null, "", href(path, next));
  dispatchEvent(new HashChangeEvent("hashchange"));
}

export const deploymentHref = (namespace: string, name: string, tab?: string) => href(["deployments", namespace, name, tab]);
export const traceHref = (traceId: string, span?: string) => href(["trace", traceId], { span });

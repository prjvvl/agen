// Client for the Hub API (Connect JSON: POST /agen.v1.HubService/<Method>).
// The UI uses the same API, tokens and scopes as the CLI and MCP.

const TOKEN_KEY = "agen.token";

export function token(): string {
  try {
    return sessionStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setToken(t: string) {
  try {
    if (t) sessionStorage.setItem(TOKEN_KEY, t);
    else sessionStorage.removeItem(TOKEN_KEY);
  } catch {
    /* storage unavailable: token lives for this page only */
  }
}

// A login link (`agen ui`) carries the token in the URL fragment, which is
// never sent to a server; take it and remove it from the address bar.
export function tokenFromFragment() {
  const m = /[#&]token=([^&]+)/.exec(location.hash);
  if (m) {
    setToken(decodeURIComponent(m[1]));
    history.replaceState(null, "", location.pathname + "#/");
    // replaceState fires no event: let the router see the cleaned route.
    dispatchEvent(new HashChangeEvent("hashchange"));
  }
}

export class ApiError extends Error {
  constructor(
    message: string,
    public code: string,
    public status: number,
  ) {
    super(message);
  }
}

export async function call<T>(method: string, body: object = {}): Promise<T> {
  const r = await fetch(`/agen.v1.HubService/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token()}` },
    body: JSON.stringify(body),
  });
  const text = await r.text();
  let data: Record<string, unknown> = {};
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    /* not JSON */
  }
  if (!r.ok) {
    if (r.status === 401) {
      // Expired or revoked token: back to sign-in.
      setToken("");
      dispatchEvent(new Event("agen:signed-out"));
    }
    throw new ApiError(String(data.message ?? r.statusText), String(data.code ?? "unknown"), r.status);
  }
  return data as T;
}

// ---- API types (protojson field names) ----

export interface Ref {
  namespace?: string;
  name: string;
}

export interface ScalePolicy {
  min?: number;
  max?: number;
  targetQueuePerInstance?: number;
  idleTimeoutSeconds?: number;
  maxConcurrency?: number;
}

export interface Deployment {
  namespace: string;
  name: string;
  definitionDigest: string;
  kind: string;
  scale?: ScalePolicy;
  desired?: number;
  ready?: number;
  paused?: boolean;
  budgetExhausted?: boolean;
  spentUsdToday?: number;
  budget?: { maxUsdPerDay?: number };
  triggers?: { type: string; name: string; schedule?: string }[];
}

export interface Instance {
  id: string;
  namespace: string;
  deployment: string;
  nestId: string;
  state: string;
  endpoint?: string;
  runningTasks?: number;
  definitionDigest?: string;
}

export interface Nest {
  id: string;
  name: string;
  backend: string;
  state: string;
  capacity?: number;
  used?: number;
  lastHeartbeat?: string;
}

export interface Task {
  id: string;
  namespace: string;
  deployment: string;
  input?: string;
  state: string;
  output?: string;
  error?: string;
  runId?: string;
  source?: string;
  createdAt?: string;
}

export interface Run {
  id: string;
  namespace: string;
  deployment: string;
  status: string;
  input?: string;
  output?: string;
  traceId?: string;
  parentRunId?: string;
  rootRunId?: string;
  startedAt?: string;
  usage?: { inputTokens?: string; outputTokens?: string; costUsd?: number };
}

export interface Span {
  spanId: string;
  parentSpanId?: string;
  name: string;
  runId: string;
  start?: string;
  end?: string;
  status: string;
  attributes?: Record<string, unknown>;
}

export interface Approval {
  id: string;
  namespace: string;
  deployment: string;
  runId: string;
  tool: string;
  arguments?: Record<string, unknown>;
  state: string;
  requestedBy?: string;
  decidedBy?: string;
  expiresAt?: string;
}

export interface LogLine {
  instanceId: string;
  time: string;
  level: string;
  message: string;
}

export interface TriggerEvent {
  trigger: string;
  state: string;
  dueAt?: string;
  taskId?: string;
  message?: string;
}

export const short = (s?: string, n = 12) => (s ?? "").replace(/^sha256:/, "").slice(0, n);
export const lower = (s: string, prefix: string) => s.replace(prefix, "").toLowerCase();

export function b64(text: string): string {
  return bytesB64(new TextEncoder().encode(text));
}

// Base64 of raw bytes, in chunks (spreading a large array into
// String.fromCharCode overflows the call stack).
export function bytesB64(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(bin);
}

// Text of base64 content, or undefined when it is not UTF-8 text that
// survives a round trip (edit it as binary: keep it unchanged).
export function textOf(s: string): string | undefined {
  try {
    const t = new TextDecoder("utf-8", { fatal: true }).decode(Uint8Array.from(atob(s), (c) => c.charCodeAt(0)));
    return b64(t) === s ? t : undefined;
  } catch {
    return undefined;
  }
}

export function unb64(s: string): string {
  const bin = atob(s);
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  return new TextDecoder().decode(bytes);
}

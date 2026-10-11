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

// ---- API types (protojson field names; int64 values arrive as strings) ----

export type Int = string | number;
export const num = (v?: Int) => Number(v ?? 0);

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

export interface Limits {
  maxDelegationDepth?: number;
  maxFanOut?: number;
  maxTotalDelegations?: number;
  maxQueuedTasks?: number;
}

export interface Trigger {
  type: string;
  name: string;
  schedule?: string;
  input?: string;
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
  budget?: { maxTokensPerRun?: Int; maxUsdPerRun?: number; maxUsdPerDay?: number };
  limits?: Limits;
  triggers?: Trigger[];
  createdAt?: string;
  updatedAt?: string;
  lastActivityUnix?: Int;
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
  startedAt?: string;
  lastSeen?: string;
  message?: string;
  tools?: string[];
  toolServers?: { name: string; state: string; toolCount?: number }[];
}

export interface Nest {
  id: string;
  name: string;
  backend: string;
  state: string;
  capacity?: number;
  used?: number;
  lastHeartbeat?: string;
  labels?: Record<string, string>;
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
  attempts?: number;
  createdAt?: string;
  updatedAt?: string;
  traceId?: string;
  conversationKey?: string;
  labels?: Record<string, string>;
  pendingApprovalId?: string;
}

export interface Usage {
  inputTokens?: Int;
  outputTokens?: Int;
  costUsd?: number;
}

export interface Run {
  id: string;
  namespace: string;
  deployment: string;
  status: string;
  input?: string;
  output?: string;
  error?: string;
  traceId?: string;
  parentRunId?: string;
  rootRunId?: string;
  taskId?: string;
  definitionDigest?: string;
  conversationId?: string;
  startedAt?: string;
  endedAt?: string;
  usage?: Usage;
  steps?: Int;
  labels?: Record<string, string>;
  treeUsage?: Usage;
  treeRuns?: number;
}

export interface Span {
  traceId?: string;
  spanId: string;
  parentSpanId?: string;
  name: string;
  runId: string;
  start?: string;
  end?: string;
  status: string;
  attributes?: Record<string, unknown>;
}

export interface ToolCallMessage {
  id: string;
  name: string;
  arguments?: unknown;
}

export interface TranscriptMessage {
  seq: Int;
  runId: string;
  role: string;
  content?: string;
  toolCalls?: ToolCallMessage[];
  toolCallId?: string;
  createdAt?: string;
}

export interface Transcript {
  run?: Run;
  messages?: TranscriptMessage[];
  systemPrompt?: string;
}

export interface Approval {
  id: string;
  namespace: string;
  deployment: string;
  runId: string;
  taskId?: string;
  tool: string;
  arguments?: Record<string, unknown>;
  state: string;
  requestedBy?: string;
  requestedByName?: string;
  decidedBy?: string;
  decidedByName?: string;
  createdAt?: string;
  expiresAt?: string;
  decidedAt?: string;
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

export interface MetricsBucket {
  runs?: number;
  failed?: number;
  costUsd?: number;
}

export interface DeploymentMetrics {
  ref: Ref;
  runs?: number;
  failed?: number;
  cancelled?: number;
  active?: number;
  p50Ms?: Int;
  p95Ms?: Int;
  usage?: Usage;
  buckets?: MetricsBucket[];
}

export interface CallEdge {
  from: Ref;
  to: Ref;
  calls?: number;
  failed?: number;
}

export interface Metrics {
  deployments?: DeploymentMetrics[];
  edges?: CallEdge[];
  since?: string;
  bucketSeconds?: number;
}

export interface Template {
  name: string;
  title: string;
  description: string;
  category: string;
  secrets?: string[];
  files?: Record<string, string>;
  tools?: string[];
  skills?: string[];
}

export interface NotificationTarget {
  namespace: string;
  name: string;
  url: string;
  events?: string[];
  createdAt?: string;
}

export interface ApiToken {
  id: string;
  name: string;
  scopes?: string[];
  namespaces?: string[];
  createdAt?: string;
  expiresAt?: string;
  revoked?: boolean;
  onBehalf?: boolean;
}

export interface SecretInfo {
  namespace: string;
  name: string;
  updatedAt?: string;
  deployments?: string[];
}

export interface Definition {
  name: string;
  digest: string;
  files?: Record<string, string>;
  createdAt?: string;
}

export interface WhoAmI {
  id: string;
  name?: string;
  scopes?: string[];
  namespaces?: string[];
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

export function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

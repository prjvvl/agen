export interface Provider {
  type: 'fake' | 'openrouter' | 'openai';
  [k: string]: unknown;
}

export function fake(...responses: object[]): Provider;
export namespace fake {
  function cycle(...responses: object[]): Provider;
}
export function openrouter(apiKey?: string, baseUrl?: string): Provider;
export function openai(apiKey?: string, baseUrl?: string): Provider;

export interface ToolContext {
  /** Aborted if the run is cancelled, the call is abandoned, or the agent closes. */
  signal: AbortSignal;
}

export interface Tool<A = any> {
  name: string;
  description?: string;
  /** JSON Schema for the arguments object. */
  parameters?: object;
  /** Read-only tools skip the effect ledger. Default false (side-effecting). */
  readOnly?: boolean;
  timeoutSeconds?: number;
  handler: (args: A, ctx: ToolContext) => unknown | Promise<unknown>;
}

export function tool<A = any>(t: Tool<A>): Tool<A>;

export interface AgentOptions {
  bundle?: string;
  name?: string;
  instructions?: string;
  model?: string;
  provider?: Provider;
  tools?: Tool[];
  approve?: (tool: string, args: any, ctx: ToolContext) => boolean | Promise<boolean>;
  store?: string;
  secrets?: Record<string, string>;
  permissions?: object;
  maxTurns?: number;
  maxOutputTokens?: number;
  /** Models tried in order when the model keeps failing or gives no answer. */
  fallbackModels?: string[];
  /** A routing model's settings (model "typesafe/jev-router", "openrouter/auto" or "openrouter/auto-beta"). */
  router?: { tier?: string; maxTier?: string; budgetTier?: string; allowedModels?: string[]; excludedModels?: string[] };
  reasoningEffort?: "minimal" | "low" | "medium" | "high";
  /** OpenRouter request settings sent as given. */
  openrouter?: { provider?: Record<string, unknown>; plugins?: Record<string, unknown>[] };
  /** About how many tokens of conversation one model call may send. */
  contextTokens?: number;
  /** A run that uses more tokens than this fails (model calls across all turns). */
  maxTokensPerRun?: number;
  temperature?: number;
  approvalTimeoutSeconds?: number;
  namespace?: string;
  deployment?: string;
}

export interface RunOptions {
  onDelta?: (text: string) => void;
  onReset?: () => void;
  sessionId?: string;
  taskId?: string;
  newConversation?: boolean;
  traceparent?: string;
  /** For a routing model: this run's cost tier, capped at the agent's maxTier. */
  modelTier?: "low" | "medium" | "high" | "xhigh" | "max";
  signal?: AbortSignal;
}

export interface RunResult {
  runId: string;
  sessionId: string;
  conversationId: string;
  status: 'succeeded' | 'failed' | 'cancelled';
  ok: boolean;
  output: string;
  error: string;
  usage: { inputTokens: number; outputTokens: number; costUsd: number };
  traceId: string;
}

export interface Span {
  spanId: string;
  parentSpanId: string;
  name: string;
  runId: string;
  startMs: number;
  endMs: number;
  status: string;
  attributes: Record<string, unknown>;
}

export class Agent {
  constructor(opts: AgentOptions);
  run(input: string, opts?: RunOptions): Promise<RunResult>;
  trace(traceId: string): Promise<Span[]>;
  close(): void;
}

export class AgenError extends Error {
  code: string;
}

export function version(): string;

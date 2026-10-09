'use strict';
// Embed the Agen agent engine in Node.js. The engine is the same Rust engine
// the Agen platform runs; this module is a thin wrapper over its native addon.

const { NativeAgent, version } = require('./agen.node');

class AgenError extends Error {
  constructor(code, message) {
    super(`${code}: ${message}`);
    this.name = 'AgenError';
    this.code = code;
  }

  static fromNative(e) {
    try {
      const d = JSON.parse(e.message);
      return new AgenError(d.code || 'internal', d.message || '');
    } catch {
      return new AgenError('internal', String(e && e.message ? e.message : e));
    }
  }
}

// ---- providers ----

/** Scripted responses: `{text}` or `{toolCalls: [{name, arguments}]}`. */
function fake(...responses) {
  return { type: 'fake', responses };
}
fake.cycle = (...responses) => ({ type: 'fake', responses, cycle: true });

/** OpenRouter; without apiKey reads $OPENROUTER_API_KEY. */
function openrouter(apiKey, baseUrl) {
  return { type: 'openrouter', ...(apiKey && { apiKey }), ...(baseUrl && { baseUrl }) };
}

/** OpenAI directly; without apiKey reads $OPENAI_API_KEY. */
function openai(apiKey, baseUrl) {
  return { type: 'openai', ...(apiKey && { apiKey }), ...(baseUrl && { baseUrl }) };
}

/**
 * Define a tool. `handler(args, { signal })` may be async. Tools are
 * side-effecting unless `readOnly: true`; side-effecting calls go through the
 * engine's effect ledger and are never silently repeated.
 */
function tool({ name, description = '', parameters = { type: 'object' }, readOnly = false, timeoutSeconds, handler }) {
  if (!name || typeof handler !== 'function') throw new TypeError('tool needs a name and a handler');
  return { name, description, parameters, readOnly, timeoutSeconds, handler };
}

function toResultString(out) {
  if (typeof out === 'string') return out;
  const s = JSON.stringify(out);
  if (s === undefined) throw new TypeError(`tool returned ${typeof out}, which is not JSON`);
  return s;
}

let keySeq = 0;

class Agent {
  /**
   * @param {object} opts `name` + `provider` (+ `instructions`, `model`), or `bundle`.
   */
  constructor(opts = {}) {
    const { tools = [], approve, ...rest } = opts;
    this._tools = new Map(tools.map((t) => [t.name, t]));
    this._approve = approve;
    this._inflight = new Map(); // request id -> AbortController
    this._closed = false;
    const spec = {
      bundle: rest.bundle,
      name: rest.name,
      instructions: rest.instructions,
      model: rest.model,
      provider: rest.provider,
      store: rest.store,
      secrets: rest.secrets,
      permissions: rest.permissions,
      maxTurns: rest.maxTurns,
      maxOutputTokens: rest.maxOutputTokens,
      temperature: rest.temperature,
      approvalTimeoutSeconds: rest.approvalTimeoutSeconds,
      namespace: rest.namespace,
      deployment: rest.deployment,
      tools: tools.map((t) => ({
        name: t.name,
        description: t.description,
        parameters: t.parameters,
        sideEffect: !t.readOnly,
        ...(t.timeoutSeconds && { timeoutSeconds: t.timeoutSeconds }),
      })),
      hostApprovals: typeof approve === 'function',
    };
    for (const k of Object.keys(spec)) if (spec[k] === undefined) delete spec[k];
    // Hold only a WeakRef in the native callback so the Agent can be collected.
    const ref = new WeakRef(this);
    try {
      this._native = new NativeAgent(JSON.stringify(spec), (req) => ref.deref()?._onHost(req));
    } catch (e) {
      throw AgenError.fromNative(e);
    }
  }

  _onHost(requestJson) {
    let req;
    try {
      req = JSON.parse(requestJson);
    } catch {
      return;
    }
    if (req.kind === 'cancel') {
      this._inflight.get(req.id)?.abort(new Error('cancelled by the engine'));
      return;
    }
    const ac = new AbortController();
    this._inflight.set(req.id, ac);
    // Every request must be answered, whatever happens.
    Promise.resolve()
      .then(async () => {
        if (req.kind === 'tool') {
          const t = this._tools.get(req.name);
          if (!t) throw new Error(`no JS implementation for tool ${req.name}`);
          return toResultString(await t.handler(req.arguments ?? {}, { signal: ac.signal }));
        }
        if (req.kind === 'approval') {
          const ok = this._approve ? await this._approve(req.tool, req.arguments ?? {}, { signal: ac.signal }) : false;
          return ok ? 'approved' : 'denied';
        }
        throw new Error(`unknown request kind ${req.kind}`);
      })
      .then(
        (out) => this._native.complete(req.id, out, false),
        (err) => this._native.complete(req.id, `${err && err.name ? err.name : 'Error'}: ${err && err.message ? err.message : err}`, true),
      )
      .finally(() => this._inflight.delete(req.id));
  }

  /**
   * Run to completion.
   * @param {string} input
   * @param {{onDelta?, onReset?, sessionId?, taskId?, newConversation?, traceparent?, signal?: AbortSignal}} [opts]
   */
  async run(input, opts = {}) {
    if (this._closed) throw new AgenError('closed', 'agent is closed');
    const key = `js-${++keySeq}`;
    const options = { cancelKey: key };
    if (opts.sessionId) options.sessionId = opts.sessionId;
    if (opts.taskId) options.taskId = opts.taskId;
    if (opts.newConversation) options.newConversation = true;
    if (opts.traceparent) options.traceparent = opts.traceparent;
    const onEvent = (evJson) => {
      const ev = JSON.parse(evJson);
      if (ev.type === 'delta') opts.onDelta?.(ev.text);
      else if (ev.type === 'reset') opts.onReset?.();
    };
    const signal = opts.signal;
    const onAbort = () => this._native.cancel(key);
    if (signal) {
      if (signal.aborted) onAbort(); // the engine remembers an early cancel
      else signal.addEventListener('abort', onAbort, { once: true });
    }
    try {
      const out = await this._native.run(input, JSON.stringify(options), opts.onDelta || opts.onReset ? onEvent : undefined);
      return fromResult(JSON.parse(out));
    } catch (e) {
      throw AgenError.fromNative(e);
    } finally {
      signal?.removeEventListener('abort', onAbort);
    }
  }

  /** Spans of a run's trace (`result.traceId`). */
  async trace(traceId) {
    try {
      return JSON.parse(await this._native.trace(traceId));
    } catch (e) {
      throw AgenError.fromNative(e);
    }
  }

  /** Release the engine (store, MCP servers). Idempotent. */
  close() {
    if (this._closed) return;
    this._closed = true;
    for (const ac of this._inflight.values()) ac.abort(new Error('agent closed'));
    this._native.close();
  }
}

function fromResult(d) {
  return {
    runId: d.runId,
    sessionId: d.sessionId,
    conversationId: d.conversationId,
    status: d.status,
    ok: d.status === 'succeeded',
    output: d.output,
    error: d.error,
    usage: d.usage,
    traceId: d.traceId,
  };
}

module.exports = { Agent, AgenError, tool, fake, openrouter, openai, version };

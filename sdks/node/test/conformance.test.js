'use strict';
// Shared SDK conformance suite (spec/conformance/cases) for the Node SDK.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { Agent, AgenError, tool } = require('..');

const ROOT = path.join(__dirname, '..', '..', '..');
const DIR = path.join(ROOT, 'spec', 'conformance', 'cases');
const CASES = fs.readdirSync(DIR).filter((f) => f.endsWith('.json')).sort();

const KEYS = {
  case: ['name', 'description', 'agent', 'tools', 'approvals', 'runs', 'expect', 'createError'],
  agent: ['name', 'bundle', 'instructions', 'model', 'provider', 'permissions', 'maxTurns', 'approvalTimeoutSeconds'],
  tool: ['name', 'readOnly', 'timeoutSeconds', 'behavior'],
  run: ['input', 'options', 'sameSession', 'cancelAfterMs', 'cancelBeforeStart', 'closeAfterMs', 'concurrent'],
  options: ['taskId'],
  expect: ['status', 'output', 'minDeltas', 'deltasEqualOutput', 'toolCalls', 'approvalsAsked', 'toolCancelled',
    'sameConversationAsPrevious', 'sameRunAsPrevious', 'maxWallMs', 'traceIncludes'],
};

function strict(obj, kind) {
  const unknown = Object.keys(obj || {}).filter((k) => !KEYS[kind].includes(k));
  assert.deepEqual(unknown, [], `${kind}: unknown keys (update the runner)`);
}

function makeTool(spec, state) {
  strict(spec, 'tool');
  const b = spec.behavior;
  return tool({
    name: spec.name,
    readOnly: !!spec.readOnly,
    timeoutSeconds: spec.timeoutSeconds,
    handler: async (args, { signal }) => {
      state.calls.push(spec.name);
      if ('return' in b) return b.return;
      if (b.echoArgs) return JSON.stringify(args);
      if ('error' in b) throw new Error(b.error);
      if ('sleepMs' in b) {
        await new Promise((resolve) => {
          const t = setTimeout(resolve, b.sleepMs);
          signal.addEventListener('abort', () => { clearTimeout(t); state.toolCancelled = true; resolve(); }, { once: true });
        });
        return 'slept';
      }
      if (b.nonJson) return undefined;
      throw new Error(`unknown behavior ${JSON.stringify(b)}`);
    },
  });
}

function agentOptions(spec) {
  strict(spec, 'agent');
  const o = { ...spec };
  if (o.bundle) o.bundle = path.join(ROOT, o.bundle);
  return o;
}

async function runOnce(agent, run, prev) {
  strict(run.options, 'options');
  const deltas = [];
  const opts = { onDelta: (d) => deltas.push(d), onReset: () => (deltas.length = 0) };
  if (run.options?.taskId) opts.taskId = run.options.taskId;
  if (run.sameSession) opts.sessionId = prev.sessionId;
  const ac = new AbortController();
  if (run.cancelBeforeStart) ac.abort();
  if (run.cancelAfterMs !== undefined) setTimeout(() => ac.abort(), run.cancelAfterMs);
  if (run.closeAfterMs !== undefined) setTimeout(() => agent.close(), run.closeAfterMs);
  opts.signal = ac.signal;
  const r = await agent.run(run.input, opts);
  return { ...r, deltas };
}

async function check(agent, exp, got, prev, state, callsBefore, approvalsBefore, single) {
  assert.equal(got.status, exp.status, JSON.stringify(got));
  if ('output' in exp) assert.equal(got.output, exp.output);
  if ('minDeltas' in exp) assert.ok(got.deltas.length >= exp.minDeltas, `deltas: ${got.deltas.length}`);
  if (exp.deltasEqualOutput) assert.equal(got.deltas.join(''), got.output);
  if ('toolCalls' in exp && single) assert.deepEqual(state.calls.slice(callsBefore), exp.toolCalls);
  if ('approvalsAsked' in exp) assert.deepEqual(state.approvals.slice(approvalsBefore), exp.approvalsAsked);
  if (exp.toolCancelled) {
    for (let i = 0; i < 30 && !state.toolCancelled; i++) await new Promise((r) => setTimeout(r, 100));
    assert.ok(state.toolCancelled, 'tool never saw the cancel');
  }
  if (exp.sameConversationAsPrevious) assert.equal(got.conversationId, prev.conversationId);
  if (exp.sameRunAsPrevious) assert.equal(got.runId, prev.runId);
  if (exp.traceIncludes) {
    const names = new Set((await agent.trace(got.traceId)).map((s) => s.name));
    for (const n of exp.traceIncludes) assert.ok(names.has(n), `trace lacks ${n}: ${[...names]}`);
  }
}

for (const file of CASES) {
  const c = JSON.parse(fs.readFileSync(path.join(DIR, file), 'utf8'));
  test(`conformance: ${c.name}`, async () => {
    strict(c, 'case');
    const state = { calls: [], approvals: [], toolCancelled: false };
    const tools = (c.tools || []).map((t) => makeTool(t, state));
    let approve;
    assert.ok([undefined, 'approve', 'deny', 'hang'].includes(c.approvals), c.approvals);
    if (c.approvals) {
      approve = async (t, _args, { signal }) => {
        state.approvals.push(t);
        if (c.approvals === 'hang') {
          await new Promise((resolve) => { const h = setTimeout(resolve, 5000); signal.addEventListener('abort', () => { clearTimeout(h); resolve(); }); });
        }
        return c.approvals === 'approve';
      };
    }
    const opts = { ...agentOptions(c.agent), tools, approve };
    if (c.createError) {
      assert.throws(() => new Agent(opts), (e) => e instanceof AgenError && e.code === c.createError);
      return;
    }
    const agent = new Agent(opts);
    try {
      assert.equal(c.runs.length, c.expect.length);
      let prev;
      for (let i = 0; i < c.runs.length; i++) {
        const run = c.runs[i];
        const exp = c.expect[i];
        strict(run, 'run');
        strict(exp, 'expect');
        const n = run.concurrent || 1;
        const callsBefore = state.calls.length;
        const approvalsBefore = state.approvals.length;
        const started = Date.now();
        const results = await Promise.all(Array.from({ length: n }, () => runOnce(agent, run, prev)));
        const wall = Date.now() - started;
        if ('maxWallMs' in exp) assert.ok(wall <= exp.maxWallMs, `took ${wall} ms > ${exp.maxWallMs} ms`);
        for (const got of results) await check(agent, exp, got, prev, state, callsBefore, approvalsBefore, n === 1);
        prev = results[results.length - 1];
      }
    } finally {
      agent.close();
    }
  });
}

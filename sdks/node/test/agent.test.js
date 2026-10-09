'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { Agent, AgenError, tool, fake } = require('..');

const HELLO = path.join(__dirname, '..', '..', '..', 'examples', 'bundles', 'hello');
const call = (name, args = {}) => ({ toolCalls: [{ name, arguments: args }] });
const say = (text, expect, extra = {}) => ({ text, ...(expect && { expect }), ...extra });

const add = tool({
  name: 'add',
  description: 'Add a and b',
  readOnly: true,
  parameters: { type: 'object', properties: { a: { type: 'integer' }, b: { type: 'integer' } } },
  handler: ({ a, b }) => a + b,
});

test('JS tool round trip and streaming', async () => {
  const agent = new Agent({ name: 'calc', provider: fake(call('add', { a: 2, b: 3 }), say('The answer is 5', '5')), tools: [add] });
  let streamed = '';
  const r = await agent.run('2+3?', { onDelta: (s) => (streamed += s) });
  assert.equal(r.status, 'succeeded');
  assert.equal(r.output, 'The answer is 5');
  assert.equal(streamed, 'The answer is 5');
  assert.ok(r.runId && r.traceId && r.usage.inputTokens > 0);
  agent.close();
});

test('async tools, thrown errors, rejections and non-JSON results reach the model', async () => {
  const later = tool({ name: 'later', handler: async ({ x }) => { await new Promise((r) => setTimeout(r, 10)); return `later:${x}`; } });
  const throws = tool({ name: 'throws', handler: () => { throw new RangeError('bad range'); } });
  const rejects = tool({ name: 'rejects', handler: async () => { throw new Error('nope'); } });
  const weird = tool({ name: 'weird', handler: () => undefined });
  const agent = new Agent({
    name: 't',
    provider: fake(call('later', { x: 'ok' }), call('throws'), call('rejects'), call('weird'), say('done', 'not JSON')),
    tools: [later, throws, rejects, weird],
  });
  const r = await agent.run('go');
  assert.equal(r.output, 'done');
  agent.close();
});

test('AbortSignal cancels the run and reaches the running tool', async () => {
  let toolSawAbort = false;
  const wait = tool({
    name: 'wait',
    handler: (_a, { signal }) => new Promise((resolve) => signal.addEventListener('abort', () => { toolSawAbort = true; resolve('stopped'); })),
  });
  const agent = new Agent({ name: 'c', provider: fake(call('wait')), tools: [wait] });
  const r = await agent.run('x', { signal: AbortSignal.timeout(300) });
  assert.equal(r.status, 'cancelled');
  await new Promise((r2) => setTimeout(r2, 100));
  assert.ok(toolSawAbort, 'tool never saw the cancel');
  agent.close();
});

test('an already-aborted signal starts the run cancelled', async () => {
  const agent = new Agent({ name: 'c', provider: fake(say('never', null, { delayMs: 10000 })) });
  const ac = new AbortController();
  ac.abort();
  const r = await agent.run('x', { signal: ac.signal });
  assert.equal(r.status, 'cancelled');
  agent.close();
});

test('bundle agents and approvals', async () => {
  const hello = new Agent({ bundle: HELLO });
  assert.equal((await hello.run('hi')).output, 'Hello! Nice to meet you.');
  hello.close();

  const asked = [];
  const pay = tool({ name: 'pay', handler: ({ to }) => `paid ${to}` });
  const agent = new Agent({
    name: 'payer',
    permissions: { default: 'ask' },
    provider: fake(call('pay', { to: 'bob' }), say('ok', 'paid bob')),
    tools: [pay],
    approve: async (t, args) => { asked.push([t, args]); return true; },
  });
  assert.equal((await agent.run('pay bob')).output, 'ok');
  assert.deepEqual(asked, [['pay', { to: 'bob' }]]);
  agent.close();
});

test('sessions, concurrent runs, closed agent and error codes', async () => {
  const agent = new Agent({ name: 'm', provider: fake.cycle(say('reply', null, { delayMs: 20 })) });
  const r1 = await agent.run('first');
  const r2 = await agent.run('second', { sessionId: r1.sessionId });
  assert.equal(r2.conversationId, r1.conversationId);
  const many = await Promise.all(Array.from({ length: 10 }, () => agent.run('x')));
  assert.ok(many.every((r) => r.status === 'succeeded'));
  agent.close();
  agent.close(); // idempotent
  await assert.rejects(agent.run('x'), (e) => e instanceof AgenError && e.code === 'closed');
  assert.throws(() => new Agent({ name: 'x' }), (e) => e instanceof AgenError && e.code === 'invalid_spec');
});

test('the event loop stays free while a run is in flight', async () => {
  const agent = new Agent({ name: 's', provider: fake(say('done', null, { delayMs: 300 })) });
  let ticks = 0;
  const timer = setInterval(() => ticks++, 10);
  await agent.run('x');
  clearInterval(timer);
  assert.ok(ticks > 10, `event loop was blocked (ticks=${ticks})`);
  agent.close();
});

test('an idle agent does not keep the process alive', () => {
  const { spawnSync } = require('node:child_process');
  const script = `
    const { Agent, tool, fake } = require(${JSON.stringify(path.join(__dirname, '..'))});
    const t = tool({ name: 't', handler: () => 'ok' });
    const a = new Agent({ name: 'x', provider: fake({ toolCalls: [{ name: 't', arguments: {} }] }, { text: 'bye' }), tools: [t] });
    a.run('go').then((r) => console.log(r.output)); // note: no close()
  `;
  const r = spawnSync(process.execPath, ['-e', script], { timeout: 15000, encoding: 'utf8' });
  assert.equal(r.signal, null, 'process had to be killed: it did not exit on its own');
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout.trim(), 'bye');
});

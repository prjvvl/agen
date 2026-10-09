// Order desk: an embedded Agen agent with a custom JS tool.
//   node app.js           scripted model, no API key needed
//   node app.js --live    real model via OpenRouter ($OPENROUTER_API_KEY)
const { Agent, tool, fake, openrouter } = require('../../sdks/node');

const ORDERS = { 'A-1001': { status: 'shipped', carrier: 'UPS', date: '2026-09-25' } };

const lookupOrder = tool({
  name: 'lookup_order',
  description: 'Look up an order by id. Returns its status, carrier and ship date.',
  readOnly: true,
  parameters: { type: 'object', properties: { order_id: { type: 'string' } }, required: ['order_id'] },
  handler: async ({ order_id }) => ORDERS[order_id] ?? { error: `no order ${order_id}` },
});

async function main() {
  const live = process.argv.includes('--live');
  const provider = live
    ? openrouter()
    : fake(
        { toolCalls: [{ name: 'lookup_order', arguments: { order_id: 'A-1001' } }] },
        { expect: 'shipped', text: 'Order A-1001 shipped on 2026-09-25 via UPS.' },
      );
  const agent = new Agent({
    name: 'order-desk',
    instructions: 'You answer order questions. Always use lookup_order, then answer in one sentence.',
    model: 'deepseek/deepseek-v4-flash',
    provider,
    tools: [lookupOrder],
    maxOutputTokens: 400,
  });
  try {
    const result = await agent.run('Where is my order A-1001?', { onDelta: (s) => process.stdout.write(s) });
    console.log(`\n\nstatus=${result.status} tokens=${result.usage.inputTokens}+${result.usage.outputTokens}`);
    console.log('trace:');
    for (const s of await agent.trace(result.traceId)) {
      console.log(`  ${s.name.padEnd(16)} ${String(s.endMs - s.startMs).padStart(5)} ms  ${s.status}`);
    }
    if (!result.ok) process.exitCode = 1;
  } finally {
    agent.close();
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});

# agen-sdk (Node.js)

Embed the Agen agent engine in a Node.js app. Same Rust engine as the Agen
platform, compiled into a native addon.

```js
const { Agent, tool, openrouter } = require('agen-sdk');

const add = tool({
  name: 'add',
  description: 'Add two integers a and b',
  readOnly: true,
  parameters: { type: 'object', properties: { a: { type: 'integer' }, b: { type: 'integer' } }, required: ['a', 'b'] },
  handler: ({ a, b }) => a + b,
});

const agent = new Agent({
  name: 'calc',
  instructions: 'Use the add tool for arithmetic, then answer with just the number.',
  model: 'deepseek/deepseek-v4-flash',
  provider: openrouter(), // reads $OPENROUTER_API_KEY
  tools: [add],
});
const result = await agent.run('What is 1234 + 4321?', { onDelta: (s) => process.stdout.write(s) });
console.log(result.output, result.usage);
agent.close();
```

- `new Agent({ bundle: 'path/to/bundle' })` loads an agent bundle.
- Tool handlers may be async and get `{ signal }`, aborted when the run is
  cancelled or the call is abandoned. Tools are side-effecting unless
  `readOnly: true`.
- `run(input, { signal })` cancels with an `AbortSignal`.
- `approve: async (tool, args) => boolean` handles `ask` permissions.
- `store: 'sqlite:agen.db'` persists sessions (default: in-memory).
- Runs execute off the JS thread; an idle agent never keeps the process alive.

## Develop

```sh
node scripts/build.js      # cargo build + copy to agen.node
node --test "test/*.test.js"
```

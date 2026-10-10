# Using Agen from Claude and other MCP clients

The Hub is an MCP server at `/mcp` (streamable HTTP). Every API method is a
tool (`create_deployment`, `submit_task`, `get_task`, `decide_approval`, ...,
listed in the [API reference](../reference/api.md)), with the same scopes and
results as the API. The server's instructions explain bundles to the model,
and `get_bundle_guide` returns the JSON Schemas and a complete example bundle,
so an assistant can write and deploy agents without reading the source.

## A token for the client

Give the client its own token with only the scopes it needs, for example:

```sh
agen token create --name claude --scope operator
```

It prints the token (once); use it as `AGEN_TOKEN` below. Add
`--scope approver` only if the client should also decide approvals, and
remember that it cannot decide approvals for tasks it submitted itself. The
client can check its access with `who_am_i`.

The examples use `http://127.0.0.1:7070/mcp`, the MCP endpoint of a local
`agen up`; for another Hub use its URL with `/mcp`.

## Claude Code

```sh
claude mcp add --transport http agen http://127.0.0.1:7070/mcp --header "Authorization: Bearer $AGEN_TOKEN"
```

## Claude Desktop

Claude Desktop starts local MCP servers as commands, so bridge to the Hub with
`mcp-remote` (needs Node.js). Open Settings → Developer → Edit Config and add:

```json
{
  "mcpServers": {
    "agen": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "http://127.0.0.1:7070/mcp", "--header", "Authorization:Bearer ${AGEN_TOKEN}"],
      "env": { "AGEN_TOKEN": "<the token>" }
    }
  }
}
```

Merge it into the existing file rather than replacing it, then quit Claude
Desktop completely and start it again. (Remote connectors added in Claude's
settings run in the cloud and cannot reach a Hub on `localhost`.)

## Tips for agents driving Agen

- Send bundles as text with `bundleText` (path → file content) instead of
  base64 `bundleFiles`, and check them first with `validateOnly: true`.
- `submit_task` returns at once; `get_task` with `waitSeconds` (up to 300)
  waits for the result.
- Use `conversationKey` to continue a conversation, and `labels` to group
  the work.
- `get_deployment` shows each instance's loaded tools and tool-server state;
  `get_logs` shows instance and task lifecycle.

package apidesc

// Primer is the short guide to Agen for API and MCP clients.
const Primer = `Agen runs agents from bundles. A deployment is a bundle running in a namespace; tasks submitted to it run on its instances.

Bundle layout (paths relative to the bundle root):
- plugin.json: {"name": "<deployment name>", ...}
- x-agen/agent.md: YAML frontmatter (name, description, optional maxTurns) and the system prompt as the body.
- x-agen/harness.json: the model, e.g. {"provider": "openrouter", "model": "deepseek/deepseek-v4-flash"}. Provider "fake" is scripted and never calls a model.
- x-agen/secrets.json: secrets the agent needs, e.g. {"OPENROUTER_API_KEY": {"source": "env"}} or {"source": "platform"} for a value stored with set_secret (admin).
- x-agen/config.json (optional): kind (pool, singleton, task), scale, budget, limits, permissions, triggers, delegates and per-tool settings.
- mcp.json (optional): the agent's tools. {"mcpServers": {"<server>": {"command": "...", "args": [...]}}} or {"url": "..."}; each tool is named <server>.<tool> (the model sees <server>_<tool>; permission rules accept either).
- skills/<name>/SKILL.md (optional): instructions the agent loads on demand.

Tools come only from mcp.json servers, skills (load_skill) and delegates. An agent can hand work to another deployment only if that deployment is listed in config.json "delegates"; it then gets a call_agent tool.

Permissions: config.json permissions.default and rules (allow, ask, deny; globs such as "exchange.*"). The default is "ask", so allow the tools the agent may use freely. An "ask" waits for an approval (list_approvals, decide_approval); the principal whose task asked cannot approve it, so use a separate token with the approver scope.

Work: submit_task queues a task; get_task with waitSeconds (up to 300) waits for its result. Tasks with the same conversationKey continue one conversation; labels travel with a task to its runs, tool calls and delegated tasks.

Call create_deployment with validateOnly: true to check a bundle and see warnings before deploying, and send bundle files as text in bundleText (path -> content). get_bundle_guide returns the JSON Schemas and a complete example bundle. get_deployment shows each instance's loaded tools; get_logs shows what happened.`

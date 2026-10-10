package apidesc

// Primer is the short guide to Agen for API and MCP clients.
const Primer = `Agen runs agents from bundles. A deployment is a bundle running in a namespace; tasks submitted to it run on its instances.

Bundle layout (paths relative to the bundle root):
- plugin.json: {"name": "<deployment name>", ...}
- x-agen/agent.md: YAML frontmatter (name, description, optional maxTurns) and the system prompt as the body.
- x-agen/harness.json: the model, e.g. {"provider": "openrouter", "model": "deepseek/deepseek-v4-flash"}. Provider "fake" is scripted and never calls a model.
- x-agen/secrets.json: secrets the agent needs, e.g. {"OPENROUTER_API_KEY": {"source": "env"}} or {"source": "platform"} for a value stored with SetSecret.
- x-agen/config.json (optional): kind (pool, singleton, task), scale, budget, limits, permissions, triggers, delegates and per-tool settings.
- mcp.json (optional): the agent's tools. {"mcpServers": {"<server>": {"command": "...", "args": [...]}}} or {"url": "..."}; each tool is named <server>.<tool> (the model sees <server>_<tool>; permission rules accept either).
- skills/<name>/SKILL.md (optional): instructions the agent loads on demand.

Tools come only from mcp.json servers, skills (load_skill) and delegates. An agent can hand work to another deployment only if that deployment is listed in config.json "delegates"; it then gets a call_agent tool.

Permissions: config.json permissions.default and rules (allow, ask, deny; globs such as "exchange.*"). An "ask" waits for an approval (ListApprovals, DecideApproval); the principal whose task asked cannot approve it, so use a separate token with the approver scope.

Work: SubmitTask runs a task; GetTask with wait_seconds (up to 300) waits for it. Tasks with the same conversation_key continue one conversation; labels travel with a task to its runs, tool calls and delegated tasks.

Use CreateDeployment with validate_only to check a bundle and see warnings before deploying. Bundle files can be sent as text in bundle_text. GetBundleGuide returns the JSON Schemas and an example bundle.`

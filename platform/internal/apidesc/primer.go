package apidesc

// Primer is the short guide to Agen for API and MCP clients.
const Primer = `Agen runs agents from bundles. A deployment is a bundle running in a namespace; tasks submitted to it run on its instances.

Bundle layout (paths relative to the bundle root):
- plugin.json: {"name": "<deployment name>", ...}
- x-agen/agent.md: YAML frontmatter (name, description, optional maxTurns) and the system prompt as the body.
- x-agen/harness.json: the model, e.g. {"provider": "openrouter", "model": "deepseek/deepseek-v4-flash"}. Provider "fake" is scripted and never calls a model.
- x-agen/secrets.json: secrets the agent needs, e.g. {"OPENROUTER_API_KEY": {"source": "env"}} or {"source": "platform"} for a value stored with set_secret (admin). "key" stores it under another name: an agent that calls the Hub keeps its own token as {"AGEN_TOKEN": {"source": "platform", "key": "AGEN_TOKEN.<agent>"}}, since agents need different scopes.
- x-agen/config.json (optional): kind (pool, singleton, task), scale, budget, limits, permissions, triggers, delegates and per-tool settings. Limits have defaults that end runs: maxRunDuration 1h (approval waits excluded), modelRequestTimeout and toolTimeout 10m, maxIdenticalToolCalls 5 (the same tool with the same arguments), maxTurns 16, delegation depth 3, fan-out 20, 50 delegations per tree, maxQueuedTasks 1000; 0 turns one off.
- mcp.json (optional): the agent's tools. {"mcpServers": {"<server>": {"command": "...", "args": [...]}}} or {"url": "..."}; each tool is named <server>.<tool> (the model sees <server>_<tool>; permission rules accept either).
- skills/<name>/SKILL.md (optional): instructions the agent loads on demand.

Tools come only from mcp.json servers, skills (load_skill) and delegates. An agent can hand work to another deployment only if that deployment is listed in config.json "delegates"; it then gets a call_agent tool.

Permissions: config.json permissions.default and rules (allow, ask, deny; globs such as "exchange.*"). The default is "ask", so allow the tools the agent may use freely. An "ask" waits for an approval (list_approvals, decide_approval); the principal whose task asked cannot approve it, so use a separate token with the approver scope. A token created with onBehalf acts for the person whose task an agent works on (within the scopes both hold) and can never decide approvals.

Work: submit_task queues a task; get_task with waitSeconds (up to 300) waits for its result, and returns early with pendingApprovalId when the task starts waiting for an approval. Tasks with the same conversationKey continue one conversation; labels travel with a task to its runs, tool calls and delegated tasks.

Starting points: list_templates returns ready-made bundles (files included) to adapt and deploy. Call create_deployment with validateOnly: true to check a bundle and see warnings before deploying, and send bundle files as text in bundleText (path -> content). get_bundle_guide returns the JSON Schemas and a complete example bundle.

Investigating: get_deployment shows each instance's state, loaded tools and, for a failed instance, why it failed (e.g. a missing secret); get_logs shows what happened. list_runs filters by deployment, status (e.g. failed), labels, since and taskId, newest first (page with pageToken). A task's runId and traceId lead to get_transcript (what the run sent and received, tool calls and results included) and get_trace (every step across agents, with timings, errors and cost). get_metrics gives runs, failures, p50/p95 duration and cost per deployment over a window.`

import { useEffect, useState } from "react";
import { fitTier, ROUTER_TIERS } from "../routers";
import { b64, bytesB64, call, Deployment, errorText, textOf } from "../api";


type Files = Record<string, string>; // path -> text

const TEXT = /\.(json|md|txt|ya?ml|toml|py|js|ts|sh)$/i;

// A new agent from a short form: the minimal bundle (plugin.json, agent.md,
// harness.json, config.json) the Hub validates like any upload.
function templateFiles(f: {
  name: string;
  description: string;
  instructions: string;
  provider: string;
  model: string;
  router: string;
  tier: string;
  maxTier: string;
  fallback: string;
  reply: string;
  kind: string;
  max: number;
  idle: string;
  delayMs: number;
}): Files {
  const files: Files = {
    "plugin.json": JSON.stringify({ name: f.name, version: "0.1.0", description: f.description }, null, 2) + "\n",
    "x-agen/agent.md": `---\nname: ${f.name}\ndescription: ${f.description || f.name}\n---\n\n${f.instructions}\n`,
    "x-agen/config.json":
      JSON.stringify(
        { kind: f.kind, scale: { min: 0, max: f.kind === "singleton" ? 1 : f.max, idleTimeout: f.idle || undefined }, permissions: { default: "ask" } },
        null,
        2,
      ) + "\n",
  };
  if (f.provider === "fake") {
    files["x-agen/harness.json"] = JSON.stringify({ provider: "fake", model: "fake-1", script: "x-agen/fake-script.json" }, null, 2) + "\n";
    files["x-agen/fake-script.json"] =
      JSON.stringify({ cycle: true, responses: [{ text: f.reply, ...(f.delayMs ? { delayMs: f.delayMs } : {}) }] }, null, 2) + "\n";
  } else if (f.provider === "router") {
    // A routing model picks the serving model per request, within a tier.
    const harness = {
      provider: "openrouter",
      model: f.router,
      router: { tier: f.tier, maxTier: f.maxTier },
      ...(f.fallback ? { fallbackModels: [f.fallback] } : {}),
      maxOutputTokens: 4000,
    };
    files["x-agen/harness.json"] = JSON.stringify(harness, null, 2) + "\n";
    files["x-agen/secrets.json"] = JSON.stringify({ OPENROUTER_API_KEY: { source: "env" } }, null, 2) + "\n";
  } else {
    files["x-agen/harness.json"] = JSON.stringify({ provider: "openrouter", model: f.model }, null, 2) + "\n";
    files["x-agen/secrets.json"] = JSON.stringify({ OPENROUTER_API_KEY: { source: "env" } }, null, 2) + "\n";
  }
  return files;
}

export function BundleEditor({ namespace, name }: { namespace?: string; name?: string }) {
  const editing = !!name;
  const [files, setFiles] = useState<Files>({});
  const [binary, setBinary] = useState<Record<string, string>>({}); // path -> base64 (kept as is)
  const [selected, setSelected] = useState("");
  const [msg, setMsg] = useState("");
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({
    namespace: "default",
    name: "",
    description: "",
    instructions: "You are a helpful assistant.",
    provider: "openrouter",
    model: "deepseek/deepseek-v4-flash",
    router: "typesafe/jev-router",
    tier: "low",
    maxTier: "medium",
    fallback: "deepseek/deepseek-v4-flash",
    reply: "Hello from a new agent.",
    kind: "pool",
    max: 3,
    idle: "5m",
    delayMs: 0,
  });

  useEffect(() => {
    if (!editing) return;
    (async () => {
      try {
        const d = await call<{ deployment: Deployment }>("GetDeployment", { ref: { namespace, name } });
        const def = await call<{ definition: { files: Record<string, string> } }>("GetDefinition", { digest: d.deployment.definitionDigest });
        const text: Files = {};
        const bin: Record<string, string> = {};
        for (const [p, v] of Object.entries(def.definition.files ?? {})) {
          // Editable only if it is text that survives a round trip.
          const t = TEXT.test(p) ? textOf(v) : undefined;
          if (t !== undefined) text[p] = t;
          else bin[p] = v;
        }
        setFiles(text);
        setBinary(bin);
        setSelected(Object.keys(text).sort()[0] ?? "");
      } catch (e) {
        setMsg(errorText(e));
      }
    })();
  }, [editing, namespace, name]);

  function encoded(fs: Files) {
    const out: Record<string, string> = { ...binary };
    for (const [p, t] of Object.entries(fs)) out[p] = b64(t);
    return out;
  }

  async function save() {
    setBusy(true);
    try {
      const r = await call<{ deployment: Deployment }>("UpdateDeployment", { ref: { namespace, name }, bundleFiles: encoded(files) });
      setMsg(`Saved: new definition ${r.deployment.definitionDigest.slice(7, 19)} rolling out`);
    } catch (e) {
      setMsg(errorText(e));
    }
    setBusy(false);
  }

  async function create(fs: Files, bin: Record<string, string> = {}) {
    setBusy(true);
    try {
      const bundleFiles: Record<string, string> = { ...bin };
      for (const [p, t] of Object.entries(fs)) bundleFiles[p] = b64(t);
      const r = await call<{ deployment: Deployment }>("CreateDeployment", {
        namespace: form.namespace,
        name: form.name || undefined,
        bundleFiles,
      });
      location.hash = `#/deployments/${r.deployment.namespace}/${r.deployment.name}`;
    } catch (e) {
      setMsg(errorText(e));
    }
    setBusy(false);
  }

  async function upload(list: FileList | null) {
    if (!list?.length) return;
    const text: Files = {};
    const bin: Record<string, string> = {};
    for (const f of Array.from(list)) {
      // webkitRelativePath is "<folder>/<path>"; drop the folder.
      const rel = (f.webkitRelativePath || f.name).split("/").slice(f.webkitRelativePath ? 1 : 0).join("/");
      if (TEXT.test(rel)) text[rel] = await f.text();
      else bin[rel] = bytesB64(new Uint8Array(await f.arrayBuffer()));
    }
    await create(text, bin);
  }

  if (editing) {
    return (
      <div className="page">
        <div className="page-head">
          <div className="titles">
            <div className="crumbs">
              <a href="#/deployments">Deployments</a>
              <span>/</span>
              <a href={`#/deployments/${namespace}/${name}`}>
                {namespace}/{name}
              </a>
            </div>
            <h1>Edit bundle</h1>
            <p className="facts">Saving validates the bundle and rolls the new version out to the deployment's instances.</p>
          </div>
          <div className="actions">
            <a className="btn" href={`#/deployments/${namespace}/${name}`}>
              Back
            </a>
            <button className="btn primary" onClick={save} disabled={busy}>
              Save new version
            </button>
          </div>
        </div>
        {msg && (msg.startsWith("Saved") ? <p className="note alert" data-tone="ok">{msg}</p> : <p className="error alert">{msg}</p>)}
        <div className="editor">
          <ul className="list files" aria-label="Bundle files">
            {Object.keys(files)
              .sort()
              .map((p) => (
                <li key={p} style={{ padding: 0 }}>
                  <button className="btn ghost" aria-current={p === selected || undefined} onClick={() => setSelected(p)}>
                    <span className="truncate">{p}</span>
                  </button>
                </li>
              ))}
          </ul>
          {selected && (
            <textarea
              aria-label={`content of ${selected}`}
              spellCheck={false}
              value={files[selected]}
              onChange={(e) => setFiles({ ...files, [selected]: e.target.value })}
            />
          )}
        </div>
      </div>
    );
  }

  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) =>
    setForm({ ...form, [k]: k === "max" || k === "delayMs" ? Number(e.target.value) : e.target.value });
  return (
    <div className="page" style={{ maxWidth: 900 }}>
      <div className="page-head">
        <div className="titles">
          <div className="crumbs">
            <a href="#/deployments">Deployments</a>
          </div>
          <h1>New agent</h1>
          <p className="facts">
            Fill in the basics and the console writes the bundle. For more, <a href="#/templates">start from a template</a> or upload a bundle folder.
          </p>
        </div>
      </div>
      {msg && <p className="error alert">{msg}</p>}
      <form
        className="form-grid"
        onSubmit={(e) => {
          e.preventDefault();
          create(templateFiles(form));
        }}
      >
        <label className="field">
          Name
          <input value={form.name} onChange={set("name")} required pattern="[a-z0-9][a-z0-9-]*" aria-label="Name" />
        </label>
        <label className="field">
          Namespace
          <input value={form.namespace} onChange={set("namespace")} aria-label="Namespace" />
        </label>
        <label className="field wide">
          Description
          <input value={form.description} onChange={set("description")} aria-label="Description" />
        </label>
        <label className="field">
          Kind <span className="help">pool: many instances; singleton: one, with memory; task: one per task.</span>
          <select value={form.kind} onChange={set("kind")} aria-label="Kind">
            <option value="pool">pool</option>
            <option value="singleton">singleton</option>
            <option value="task">task</option>
          </select>
        </label>
        <label className="field">
          Max instances
          <input type="number" min={1} value={form.max} onChange={set("max")} aria-label="Max instances" />
        </label>
        <label className="field">
          Idle timeout <span className="help">Sleep after this long without work.</span>
          <input value={form.idle} onChange={set("idle")} pattern="[0-9]+(ms|s|m|h)" aria-label="Idle timeout" />
        </label>
        <label className="field">
          Model
          <select value={form.provider} onChange={set("provider")} aria-label="Model provider">
            <option value="openrouter">OpenRouter: one model</option>
            <option value="router">OpenRouter: a routing model picks the model per request</option>
            <option value="fake">Scripted test replies (never calls a model)</option>
          </select>
        </label>
        {form.provider === "router" ? (
          <>
            <label className="field">
              Routing model <span className="help">Reads OPENROUTER_API_KEY from the environment.</span>
              <select
                value={form.router}
                onChange={(e) => {
                  const r = e.target.value;
                  setForm({ ...form, router: r, tier: fitTier(r, form.tier), maxTier: fitTier(r, form.maxTier) });
                }}
                aria-label="Routing model"
              >
                <option value="typesafe/jev-router">Jev Router (typesafe/jev-router)</option>
                <option value="openrouter/auto">OpenRouter Auto Router (openrouter/auto)</option>
              </select>
            </label>
            <label className="field">
              Cost tier <span className="help">Default for each task; a task may ask for another, up to the highest tier.</span>
              <select value={form.tier} onChange={set("tier")} aria-label="Cost tier">
                {ROUTER_TIERS[form.router].map((t) => (
                  <option key={t}>{t}</option>
                ))}
              </select>
            </label>
            <label className="field">
              Highest tier a task may ask for
              <select value={form.maxTier} onChange={set("maxTier")} aria-label="Highest tier">
                {ROUTER_TIERS[form.router].map((t) => (
                  <option key={t}>{t}</option>
                ))}
              </select>
            </label>
            <label className="field">
              Fallback model <span className="help">Used when the router keeps failing. Empty: none.</span>
              <input value={form.fallback} onChange={set("fallback")} aria-label="Fallback model" />
            </label>
          </>
        ) : form.provider === "fake" ? (
          <>
            <label className="field">
              Scripted reply
              <input value={form.reply} onChange={set("reply")} aria-label="Scripted reply" />
            </label>
            <label className="field">
              Reply delay (ms)
              <input type="number" min={0} value={form.delayMs} onChange={set("delayMs")} aria-label="Reply delay" />
            </label>
          </>
        ) : (
          <label className="field">
            OpenRouter model <span className="help">Reads OPENROUTER_API_KEY from the environment.</span>
            <input value={form.model} onChange={set("model")} aria-label="OpenRouter model" />
          </label>
        )}
        <label className="field wide">
          Instructions
          <textarea value={form.instructions} onChange={set("instructions")} rows={6} aria-label="Instructions" />
        </label>
        <div className="wide actions">
          <button className="btn primary" type="submit" disabled={busy}>
            Create agent
          </button>
        </div>
      </form>
      <section className="section">
        <h2>Or upload a bundle folder</h2>
        <p className="muted">The folder with plugin.json and x-agen/. Files are validated like any deploy.</p>
        <input
          type="file"
          aria-label="Bundle folder"
          // @ts-expect-error non-standard but supported by all major browsers
          webkitdirectory=""
          multiple
          onChange={(e) => upload(e.target.files)}
        />
      </section>
    </div>
  );
}

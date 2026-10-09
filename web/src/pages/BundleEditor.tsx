import { useEffect, useState } from "react";
import { b64, bytesB64, call, Deployment, textOf } from "../api";
import { errorText } from "../hooks";

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
    provider: "fake",
    model: "deepseek/deepseek-v4-flash",
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
      <section>
        <h2>
          Edit bundle — {namespace}/{name}
        </h2>
        {msg && <p className={msg.startsWith("Saved") ? "note" : "error"}>{msg}</p>}
        <div className="editor">
          <ul className="files">
            {Object.keys(files)
              .sort()
              .map((p) => (
                <li key={p}>
                  <button className={`link ${p === selected ? "active" : ""}`} onClick={() => setSelected(p)}>
                    {p}
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
        <button onClick={save} disabled={busy}>
          Save new version
        </button>{" "}
        <a href={`#/deployments/${namespace}/${name}`}>Back</a>
      </section>
    );
  }

  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) =>
    setForm({ ...form, [k]: k === "max" || k === "delayMs" ? Number(e.target.value) : e.target.value });
  return (
    <section>
      <h2>New agent</h2>
      {msg && <p className="error">{msg}</p>}
      <form
        className="grid"
        onSubmit={(e) => {
          e.preventDefault();
          create(templateFiles(form));
        }}
      >
        <label>
          Name
          <input value={form.name} onChange={set("name")} required pattern="[a-z0-9][a-z0-9-]*" aria-label="Name" />
        </label>
        <label>
          Namespace
          <input value={form.namespace} onChange={set("namespace")} aria-label="Namespace" />
        </label>
        <label>
          Description
          <input value={form.description} onChange={set("description")} aria-label="Description" />
        </label>
        <label>
          Kind
          <select value={form.kind} onChange={set("kind")} aria-label="Kind">
            <option value="pool">pool</option>
            <option value="singleton">singleton</option>
            <option value="task">task</option>
          </select>
        </label>
        <label>
          Max instances
          <input type="number" min={1} value={form.max} onChange={set("max")} aria-label="Max instances" />
        </label>
        <label>
          Idle timeout
          <input value={form.idle} onChange={set("idle")} pattern="[0-9]+(ms|s|m|h)" aria-label="Idle timeout" />
        </label>
        <label>
          Model
          <select value={form.provider} onChange={set("provider")} aria-label="Model provider">
            <option value="fake">Scripted (no model, for testing)</option>
            <option value="openrouter">OpenRouter</option>
          </select>
        </label>
        {form.provider === "fake" ? (
          <>
            <label>
              Scripted reply
              <input value={form.reply} onChange={set("reply")} aria-label="Scripted reply" />
            </label>
            <label>
              Reply delay (ms)
              <input type="number" min={0} value={form.delayMs} onChange={set("delayMs")} aria-label="Reply delay" />
            </label>
          </>
        ) : (
          <label>
            OpenRouter model
            <input value={form.model} onChange={set("model")} aria-label="OpenRouter model" />
          </label>
        )}
        <label className="wide">
          Instructions
          <textarea value={form.instructions} onChange={set("instructions")} rows={5} aria-label="Instructions" />
        </label>
        <div className="wide">
          <button type="submit" disabled={busy}>
            Create agent
          </button>
        </div>
      </form>
      <h3>Or upload a bundle folder</h3>
      <input
        type="file"
        aria-label="Bundle folder"
        // @ts-expect-error non-standard but supported by all major browsers
        webkitdirectory=""
        multiple
        onChange={(e) => upload(e.target.files)}
      />
    </section>
  );
}

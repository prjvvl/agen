import { KeyRound, LayoutTemplate } from "lucide-react";
import { useMemo, useState } from "react";
import { call, Deployment, SecretInfo, Template } from "../api";
import { useSession } from "../App";
import { useQuery } from "../live";
import { deploymentHref, go } from "../router";
import { Alert, Badge, Empty, SkeletonRows } from "../ui";
import { PanelForm } from "./DeploymentDetail";

const CATEGORY_ORDER = ["starter", "research", "content", "support", "engineering", "operations", "multi-agent"];

export function Templates({ selected }: { selected?: string }) {
  const { can } = useSession();
  const list = useQuery(() => call<{ templates?: Template[] }>("ListTemplates"), []);
  const groups = useMemo(() => {
    const g = new Map<string, Template[]>();
    for (const t of list.data?.templates ?? []) (g.get(t.category) ?? g.set(t.category, []).get(t.category)!).push(t);
    return [...g.entries()].sort((a, b) => CATEGORY_ORDER.indexOf(a[0]) - CATEGORY_ORDER.indexOf(b[0]));
  }, [list.data]);
  const tpl = list.data?.templates?.find((t) => t.name === selected);
  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Templates</h1>
          <p className="facts">Ready-made agents to start from. Each one is an ordinary bundle you can change after it is deployed.</p>
        </div>
      </div>
      {list.error && <Alert>{list.error}</Alert>}
      {list.loading ? (
        <SkeletonRows rows={6} cols={3} />
      ) : (
        <ul className="list">
          {groups.flatMap(([category, items]) =>
            items.map((t) => (
              <li key={t.name} data-template={t.name}>
                <LayoutTemplate size={18} className="muted" aria-hidden />
                <div className="cell-main" style={{ flex: 1 }}>
                  <span className="toolbar">
                    <b>{t.title}</b>
                    <Badge>{category.replace("-", " ")}</Badge>
                  </span>
                  <span className="soft">{t.description}</span>
                  {(t.secrets ?? []).length > 0 && (
                    <span className="toolbar" style={{ marginTop: 4 }}>
                      {t.secrets!.map((s) => (
                        <Badge key={s}>
                          <KeyRound size={11} aria-hidden /> {s}
                        </Badge>
                      ))}
                    </span>
                  )}
                </div>
                {can("operator") && (
                  <a className="btn" href={`#/templates/${t.name}`} aria-label={`Use the ${t.title} template`}>
                    Use
                  </a>
                )}
              </li>
            )),
          )}
        </ul>
      )}
      {!list.loading && !groups.length && <Empty title="No templates in this build" />}
      {tpl && <DeployWizard t={tpl} onClose={() => go(["templates"])} />}
    </div>
  );
}

/** Token scopes an agent that talks to the Hub needs, per template. */
const HUB_TOKEN: Record<string, { scopes: string[]; onBehalf?: boolean }> = {
  assistant: { scopes: ["operator", "approver"], onBehalf: true },
  "approval-triage": { scopes: ["approver"] },
  "fleet-steward": { scopes: ["viewer"] },
  "cost-watchdog": { scopes: ["viewer"] },
};

function secretSources(t: Template): Record<string, string> {
  try {
    const s = JSON.parse(t.files?.["x-agen/secrets.json"] ?? "{}") as Record<string, { source?: string }>;
    return Object.fromEntries(Object.entries(s).map(([k, v]) => [k, v.source ?? "env"]));
  } catch {
    return {};
  }
}

function splitAgent(md: string): [string, string] {
  const m = /^(---[\s\S]*?\n---[ \t]*\r?\n)([\s\S]*)$/.exec(md);
  return m ? [m[1], m[2].trim()] : ["", md];
}

export function DeployWizard({ t, onClose }: { t: Template; onClose: () => void }) {
  const { can } = useSession();
  const admin = can("admin");
  const harness = useMemo(() => {
    try {
      return JSON.parse(t.files?.["x-agen/harness.json"] ?? "{}") as { provider?: string; model?: string };
    } catch {
      return {};
    }
  }, [t]);
  const [front, body] = splitAgent(t.files?.["x-agen/agent.md"] ?? "");
  const [name, setName] = useState(t.name);
  const [namespace, setNamespace] = useState("default");
  const [model, setModel] = useState(harness.model ?? "");
  const [prompt, setPrompt] = useState(body);
  const [values, setValues] = useState<Record<string, string>>({});
  const [warnings, setWarnings] = useState<string[]>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const sources = secretSources(t);
  const platform = Object.keys(sources).filter((k) => sources[k] === "platform");
  const env = Object.keys(sources).filter((k) => sources[k] === "env");
  const existing = useQuery(
    () => (admin && platform.length ? call<{ secrets?: SecretInfo[] }>("ListSecrets", { namespace }) : Promise.resolve({ secrets: [] })),
    [admin, namespace, platform.length],
  );
  const deps = useQuery(() => call<{ deployments?: Deployment[] }>("ListDeployments", { namespace }), [namespace]);
  const delegates = useMemo(() => {
    try {
      return (JSON.parse(t.files?.["x-agen/config.json"] ?? "{}").delegates ?? []).map((d: { name: string }) => d.name) as string[];
    } catch {
      return [];
    }
  }, [t]);
  const missingDelegates = delegates.filter((d) => !(deps.data?.deployments ?? []).some((x) => x.name === d));
  const has = (s: string) => (existing.data?.secrets ?? []).some((x) => x.name === s && (x.deployments ?? []).includes(name));
  const hubToken = HUB_TOKEN[t.name];

  function files(): Record<string, string> {
    const out = { ...(t.files ?? {}) };
    out["x-agen/agent.md"] = `${front}\n${prompt.trim()}\n`;
    if (model && harness.model) out["x-agen/harness.json"] = JSON.stringify({ ...harness, model }, null, 2) + "\n";
    return out;
  }

  async function setSecrets() {
    for (const s of platform) {
      let v = values[s]?.trim();
      if (!v && s === "AGEN_HUB_URL") v = location.origin;
      if (!v && s === "AGEN_TOKEN" && hubToken && !has(s)) {
        const tok = await call<{ secret: string }>("CreateApiToken", { name: `${name}-agent`, scopes: hubToken.scopes, onBehalf: hubToken.onBehalf });
        v = tok.secret;
      }
      if (v) await call("SetSecret", { namespace, name: s, value: v, deployments: [name] });
    }
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const check = await call<{ warnings?: string[] }>("CreateDeployment", { namespace, name, bundleText: files(), validateOnly: true });
      setWarnings(check.warnings ?? []);
      if (admin) await setSecrets();
      await call("CreateDeployment", { namespace, name, bundleText: files() });
      location.hash = deploymentHref(namespace, name);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  }

  return (
    <form onSubmit={submit}>
      <PanelForm title={`Deploy ${t.title}`} onClose={onClose} busy={busy} disabled={!name} submit="Deploy" wide>
        <p className="soft">{t.description}</p>
        <div className="form-grid">
          <label className="field">
            Name
            <input value={name} onChange={(e) => setName(e.target.value)} required pattern="[a-z0-9][a-z0-9-]*" aria-label="Name" />
          </label>
          <label className="field">
            Namespace
            <input value={namespace} onChange={(e) => setNamespace(e.target.value)} required aria-label="Namespace" />
          </label>
          {harness.model && (
            <label className="field wide">
              Model <span className="help">{harness.provider === "openrouter" ? "Any OpenRouter model id." : `Provider ${harness.provider}.`}</span>
              <input value={model} onChange={(e) => setModel(e.target.value)} aria-label="Model" />
            </label>
          )}
          <label className="field wide">
            Instructions <span className="help">The system prompt (x-agen/agent.md).</span>
            <textarea rows={10} value={prompt} onChange={(e) => setPrompt(e.target.value)} aria-label="Instructions" />
          </label>
        </div>
        {missingDelegates.length > 0 && (
          <Alert tone="warn">
            This agent hands work to {missingDelegates.join(" and ")}, which {missingDelegates.length === 1 ? "is" : "are"} not deployed in {namespace}. Deploy{" "}
            {missingDelegates.map((d, i) => (
              <span key={d}>
                {i > 0 && " and "}
                <a href={`#/templates/${d}`}>{d}</a>
              </span>
            ))}{" "}
            too.
          </Alert>
        )}
        {(platform.length > 0 || env.length > 0) && (
          <section className="section">
            <h3>Secrets</h3>
            {env.map((s) => (
              <p key={s} className="soft">
                <code>{s}</code> is read from the environment of the machine that runs the agent (set it before <code>agen up</code> or{" "}
                <code>agen nest run</code>).
              </p>
            ))}
            {platform.map((s) => (
              <label className="field" key={s}>
                <span>
                  <code>{s}</code> {has(s) && <Badge tone="ok">already set for {name}</Badge>}
                </span>
                {s === "AGEN_HUB_URL" ? (
                  <input value={values[s] ?? location.origin} onChange={(e) => setValues({ ...values, [s]: e.target.value })} aria-label={s} />
                ) : s === "AGEN_TOKEN" && hubToken ? (
                  <span className="help">
                    {has(s)
                      ? "Kept."
                      : `A new token with the ${hubToken.scopes.join(" and ")} scope${hubToken.scopes.length > 1 ? "s" : ""}${hubToken.onBehalf ? ", acting for whoever chats with it," : ""} is created for this agent.`}
                  </span>
                ) : (
                  <input type="password" autoComplete="off" value={values[s] ?? ""} onChange={(e) => setValues({ ...values, [s]: e.target.value })} placeholder={has(s) ? "Leave empty to keep" : ""} aria-label={s} />
                )}
              </label>
            ))}
            {!admin && platform.length > 0 && <Alert tone="info">Setting platform secrets needs an admin token; ask an admin to run agen secret set for these.</Alert>}
          </section>
        )}
        {warnings && warnings.length > 0 && (
          <Alert tone="warn">
            {warnings.map((w) => (
              <div key={w}>{w}</div>
            ))}
          </Alert>
        )}
        {error && <Alert>{error}</Alert>}
      </PanelForm>
    </form>
  );
}

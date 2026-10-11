import { Plus } from "lucide-react";
import { useState } from "react";
import { ApiToken, call, NotificationTarget, SecretInfo } from "../api";
import { useSession } from "../App";
import { useQuery } from "../live";
import { go } from "../router";
import { Alert, Badge, ConfirmButton, Copy, Empty, SkeletonRows, Tabs, Time } from "../ui";
import { PanelForm } from "./DeploymentDetail";

type Tab = "account" | "tokens" | "secrets" | "notifications";

export function Settings({ tab }: { tab?: string }) {
  const { can } = useSession();
  const admin = can("admin");
  const current = (["tokens", "secrets", "notifications"].includes(tab ?? "") && admin ? tab : "account") as Tab;
  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Settings</h1>
        </div>
      </div>
      <Tabs<Tab>
        value={current}
        onChange={(t) => go(["settings", t === "account" ? undefined : t])}
        tabs={admin ? [["account", "Your token"], ["tokens", "API tokens"], ["secrets", "Secrets"], ["notifications", "Notifications"]] : [["account", "Your token"]]}
      />
      {current === "tokens" ? <Tokens /> : current === "secrets" ? <Secrets /> : current === "notifications" ? <Notifications /> : <Account />}
    </div>
  );
}

function Account() {
  const { me } = useSession();
  if (!me) return <SkeletonRows rows={2} cols={2} />;
  return (
    <dl className="kv">
      <dt>Name</dt>
      <dd>{me.name || "admin"}</dd>
      <dt>Principal</dt>
      <dd>
        <Copy text={me.id} />
      </dd>
      <dt>Scopes</dt>
      <dd className="toolbar">
        {(me.scopes ?? []).map((s) => (
          <Badge key={s}>{s}</Badge>
        ))}
      </dd>
      <dt>Namespaces</dt>
      <dd>{(me.namespaces ?? []).length ? me.namespaces!.join(", ") : "all"}</dd>
    </dl>
  );
}

function Tokens() {
  const list = useQuery(() => call<{ tokens?: ApiToken[] }>("ListApiTokens"), []);
  const [creating, setCreating] = useState(false);
  const [secret, setSecret] = useState("");
  const [msg, setMsg] = useState("");
  const tokens = (list.data?.tokens ?? []).filter((t) => !t.name.startsWith("nest:"));
  return (
    <section className="section">
      <div className="toolbar">
        <span className="muted">Tokens for people, scripts and agents. Secrets are shown once.</span>
        <span className="grow" />
        <button className="btn primary" onClick={() => setCreating(true)}>
          <Plus size={15} aria-hidden />
          New token
        </button>
      </div>
      {secret && (
        <Alert tone="ok">
          Copy the new token now; it is not shown again: <Copy text={secret} />
        </Alert>
      )}
      {(list.error || msg) && <Alert>{list.error || msg}</Alert>}
      {list.loading ? (
        <SkeletonRows cols={5} />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Scopes</th>
                <th>Namespaces</th>
                <th>Created</th>
                <th>Expires</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.id} data-token={t.name}>
                  <td>
                    {t.name} {t.onBehalf && <Badge tone="info">acts for people</Badge>} {t.revoked && <Badge tone="err">revoked</Badge>}
                  </td>
                  <td>{(t.scopes ?? []).join(", ")}</td>
                  <td>{(t.namespaces ?? []).length ? t.namespaces!.join(", ") : "all"}</td>
                  <td>
                    <Time t={t.createdAt} />
                  </td>
                  <td>{t.expiresAt ? <Time t={t.expiresAt} /> : <span className="muted">never</span>}</td>
                  <td className="actions-cell">
                    {!t.revoked && (
                      <ConfirmButton
                        label="Revoke"
                        confirm="Revoke token"
                        className="btn sm danger"
                        onConfirm={async () => {
                          try {
                            await call("RevokeApiToken", { id: t.id });
                          } catch (e) {
                            setMsg(e instanceof Error ? e.message : String(e));
                          }
                          list.reload();
                        }}
                      />
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {creating && (
        <NewToken
          onClose={() => setCreating(false)}
          onCreated={(s) => {
            setSecret(s);
            setCreating(false);
            list.reload();
          }}
        />
      )}
    </section>
  );
}

function NewToken({ onClose, onCreated }: { onClose: () => void; onCreated: (secret: string) => void }) {
  const [name, setName] = useState("");
  const [scopes, setScopes] = useState<string[]>(["viewer"]);
  const [namespaces, setNamespaces] = useState("");
  const [days, setDays] = useState(0);
  const [error, setError] = useState("");
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      const r = await call<{ secret: string }>("CreateApiToken", {
        name,
        scopes,
        namespaces: namespaces.split(",").map((s) => s.trim()).filter(Boolean),
        ttlSeconds: days * 86400 || undefined,
      });
      onCreated(r.secret);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }
  return (
    <form onSubmit={submit}>
      <PanelForm title="New API token" onClose={onClose} submit="Create token" disabled={!name || !scopes.length}>
        <label className="field">
          Name
          <input value={name} onChange={(e) => setName(e.target.value)} required autoFocus />
        </label>
        <fieldset className="field" style={{ border: 0, padding: 0, margin: 0 }}>
          <legend style={{ marginBottom: 6 }}>Scopes</legend>
          {[
            ["viewer", "read everything in its namespaces"],
            ["operator", "deploy, scale and run tasks"],
            ["approver", "decide approvals"],
            ["admin", "everything, including tokens and secrets"],
          ].map(([s, help]) => (
            <label className="check" key={s}>
              <input type="checkbox" checked={scopes.includes(s)} onChange={(e) => setScopes(e.target.checked ? [...scopes, s] : scopes.filter((x) => x !== s))} />
              <b>{s}</b> <span className="muted">{help}</span>
            </label>
          ))}
        </fieldset>
        <label className="field">
          Namespaces <span className="help">Comma separated; empty means all.</span>
          <input value={namespaces} onChange={(e) => setNamespaces(e.target.value)} />
        </label>
        <label className="field">
          Expires after (days) <span className="help">0 never expires.</span>
          <input type="number" min={0} value={days} onChange={(e) => setDays(Number(e.target.value))} />
        </label>
        {error && <Alert>{error}</Alert>}
      </PanelForm>
    </form>
  );
}

function Secrets() {
  const [namespace, setNamespace] = useState("default");
  const list = useQuery(() => call<{ secrets?: SecretInfo[] }>("ListSecrets", { namespace }), [namespace]);
  const [adding, setAdding] = useState(false);
  const [msg, setMsg] = useState("");
  return (
    <section className="section">
      <div className="toolbar">
        <input value={namespace} onChange={(e) => setNamespace(e.target.value)} aria-label="Namespace" style={{ width: 180 }} />
        <span className="muted">Platform secrets for bundles with source "platform". Values never come back out.</span>
        <span className="grow" />
        <button className="btn primary" onClick={() => setAdding(true)}>
          <Plus size={15} aria-hidden />
          Set a secret
        </button>
      </div>
      {(list.error || msg) && <Alert>{list.error || msg}</Alert>}
      {list.loading ? (
        <SkeletonRows cols={3} />
      ) : (list.data?.secrets ?? []).length === 0 ? (
        <Empty title={`No secrets in ${namespace}`} />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Readable by</th>
                <th>Updated</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.data!.secrets!.map((s) => (
                <tr key={s.name}>
                  <td className="mono">{s.name}</td>
                  <td>{(s.deployments ?? []).join(", ")}</td>
                  <td>
                    <Time t={s.updatedAt} />
                  </td>
                  <td className="actions-cell">
                    <ConfirmButton
                      label="Delete"
                      confirm="Delete secret"
                      className="btn sm danger"
                      onConfirm={async () => {
                        try {
                          await call("DeleteSecret", { namespace, name: s.name });
                        } catch (e) {
                          setMsg(e instanceof Error ? e.message : String(e));
                        }
                        list.reload();
                      }}
                    />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {adding && (
        <SetSecret
          namespace={namespace}
          onClose={() => setAdding(false)}
          onDone={() => {
            setAdding(false);
            list.reload();
          }}
        />
      )}
    </section>
  );
}

function SetSecret({ namespace, onClose, onDone }: { namespace: string; onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [deployments, setDeployments] = useState("");
  const [error, setError] = useState("");
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await call("SetSecret", { namespace, name, value, deployments: deployments.split(",").map((s) => s.trim()).filter(Boolean) });
      onDone();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }
  return (
    <form onSubmit={submit}>
      <PanelForm title={`Set a secret in ${namespace}`} onClose={onClose} submit="Save secret" disabled={!name || !value || !deployments.trim()}>
        <label className="field">
          Name
          <input value={name} onChange={(e) => setName(e.target.value)} required autoFocus placeholder="GITHUB_TOKEN" />
        </label>
        <label className="field">
          Value
          <input type="password" autoComplete="off" value={value} onChange={(e) => setValue(e.target.value)} required />
        </label>
        <label className="field">
          Deployments that may read it <span className="help">Comma separated, at least one.</span>
          <input value={deployments} onChange={(e) => setDeployments(e.target.value)} required />
        </label>
        {error && <Alert>{error}</Alert>}
      </PanelForm>
    </form>
  );
}

function Notifications() {
  const list = useQuery(() => call<{ targets?: NotificationTarget[] }>("ListNotificationTargets", {}), []);
  const [adding, setAdding] = useState(false);
  const [secret, setSecret] = useState("");
  const [msg, setMsg] = useState("");
  return (
    <section className="section">
      <div className="toolbar">
        <span className="muted">The Hub POSTs these events as JSON, signed with X-Agen-Signature, to each target of the namespace.</span>
        <span className="grow" />
        <button className="btn primary" onClick={() => setAdding(true)}>
          <Plus size={15} aria-hidden />
          Add target
        </button>
      </div>
      {secret && (
        <Alert tone="ok">
          Signing secret (shown once): <Copy text={secret} />
        </Alert>
      )}
      {(list.error || msg) && <Alert>{list.error || msg}</Alert>}
      {list.loading ? (
        <SkeletonRows cols={4} />
      ) : (list.data?.targets ?? []).length === 0 ? (
        <Empty title="No notification targets">Add a URL (a chat webhook, an incident tool or your own service) to hear about approvals, failures and budgets.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Namespace</th>
                <th>URL</th>
                <th>Events</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.data!.targets!.map((t) => (
                <tr key={`${t.namespace}/${t.name}`}>
                  <td>{t.name}</td>
                  <td>{t.namespace}</td>
                  <td className="mono truncate" style={{ maxWidth: 320 }} title={t.url}>
                    {t.url}
                  </td>
                  <td>{(t.events ?? []).length ? t.events!.join(", ") : "all"}</td>
                  <td className="actions-cell">
                    <ConfirmButton
                      label="Remove"
                      confirm="Remove target"
                      className="btn sm danger"
                      onConfirm={async () => {
                        try {
                          await call("DeleteNotificationTarget", { namespace: t.namespace, name: t.name });
                        } catch (e) {
                          setMsg(e instanceof Error ? e.message : String(e));
                        }
                        list.reload();
                      }}
                    />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {adding && (
        <NewTarget
          onClose={() => setAdding(false)}
          onCreated={(s) => {
            setSecret(s);
            setAdding(false);
            list.reload();
          }}
        />
      )}
    </section>
  );
}

const EVENTS = [
  ["approval.pending", "an approval waits for a decision"],
  ["task.failed", "a task failed"],
  ["budget.exhausted", "a deployment used its daily budget"],
];

function NewTarget({ onClose, onCreated }: { onClose: () => void; onCreated: (secret: string) => void }) {
  const [namespace, setNamespace] = useState("default");
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [events, setEvents] = useState<string[]>(EVENTS.map((e) => e[0]));
  const [error, setError] = useState("");
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      const r = await call<{ secret: string }>("SetNotificationTarget", {
        target: { namespace, name, url, events: events.length === EVENTS.length ? [] : events },
      });
      onCreated(r.secret);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }
  return (
    <form onSubmit={submit}>
      <PanelForm title="Add a notification target" onClose={onClose} submit="Add target" disabled={!name || !url || !events.length}>
        <label className="field">
          Namespace
          <input value={namespace} onChange={(e) => setNamespace(e.target.value)} required />
        </label>
        <label className="field">
          Name
          <input value={name} onChange={(e) => setName(e.target.value)} required pattern="[a-z0-9][a-z0-9-]*" placeholder="ops-chat" />
        </label>
        <label className="field">
          URL
          <input type="url" value={url} onChange={(e) => setUrl(e.target.value)} required placeholder="https://" />
        </label>
        <fieldset className="field" style={{ border: 0, padding: 0, margin: 0 }}>
          <legend style={{ marginBottom: 6 }}>Events</legend>
          {EVENTS.map(([id, help]) => (
            <label className="check" key={id}>
              <input type="checkbox" checked={events.includes(id)} onChange={(e) => setEvents(e.target.checked ? [...events, id] : events.filter((x) => x !== id))} />
              <span className="mono">{id}</span> <span className="muted">{help}</span>
            </label>
          ))}
        </fieldset>
        {error && <Alert>{error}</Alert>}
      </PanelForm>
    </form>
  );
}

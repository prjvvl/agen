import { FileCode2, Pencil, Play } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { call, Definition, Deployment, Instance, LogLine, Metrics, num, Task, textOf, TriggerEvent } from "../api";
import { useSession } from "../App";
import { compact, duration, enumName, money, pct } from "../format";
import { useQuery } from "../live";
import { deploymentHref, go } from "../router";
import {
  Alert,
  At,
  Badge,
  ConfirmButton,
  Copy,
  deploymentStatus,
  Empty,
  instanceStatus,
  Json,
  Meter,
  Panel,
  SkeletonRows,
  Status,
  Tabs,
  Time,
} from "../ui";
import { RunsTable } from "./Runs";
import { TaskDetails, TaskPanel, TasksTable } from "./Tasks";

type Tab = "overview" | "runs" | "tasks" | "logs" | "bundle" | "triggers";

export function DeploymentDetail({ namespace, name, tab = "overview" }: { namespace: string; name: string; tab?: string }) {
  const { can } = useSession();
  const ref = { namespace, name };
  const isMine = (e: { namespace: string; deployment: string }) => e.namespace === namespace && e.deployment === name;
  const dep = useQuery(() => call<{ deployment: Deployment; instances?: Instance[] }>("GetDeployment", { ref }), [namespace, name], {
    on: (e) => (e.kind === "deployment" || e.kind === "instance") && isMine(e),
    every: 30_000,
  });
  const [msg, setMsg] = useState("");
  const [running, setRunning] = useState(false);
  const [openTask, setOpenTask] = useState<string>();

  const d = dep.data?.deployment;
  if (dep.error && !d) return <Alert>{dep.error}</Alert>;
  if (!d) return <SkeletonRows rows={3} />;
  const operator = can("operator");
  const setTab = (t: Tab) => go(["deployments", namespace, name, t === "overview" ? undefined : t]);

  async function act(f: () => Promise<unknown>) {
    try {
      await f();
      setMsg("");
      dep.reload();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <div className="crumbs">
            <a href="#/deployments">Deployments</a>
            <span>/</span>
            <span>{namespace}</span>
          </div>
          <h1>{name}</h1>
          <div className="facts">
            <span data-field="status">
              <Status spec={deploymentStatus(d)} />
            </span>
            <span>{enumName(d.kind, "DEPLOYMENT_KIND_")}</span>
            <span>
              ready <b>{d.ready ?? 0}</b> of <b>{d.desired ?? 0}</b> (scale {d.scale?.min ?? 0}–{d.scale?.max ?? 1})
            </span>
            <span className="nowrap">
              version <Copy text={d.definitionDigest} display={d.definitionDigest.replace("sha256:", "").slice(0, 12)} />
            </span>
          </div>
        </div>
        {operator && (
          <div className="actions">
            <button className="btn primary" onClick={() => setRunning(true)}>
              <Play size={15} aria-hidden />
              Run a task
            </button>
            <a className="btn" href={`#/edit/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`}>
              <Pencil size={15} aria-hidden />
              Edit bundle
            </a>
            <button className="btn" onClick={() => act(() => call("PauseDeployment", { ref, paused: !d.paused }))}>
              {d.paused ? "Start" : "Stop"}
            </button>
            <ConfirmButton
              label="Delete…"
              confirm="Delete deployment"
              className="btn danger"
              onConfirm={() =>
                act(async () => {
                  await call("DeleteDeployment", { ref });
                  location.hash = "#/deployments";
                })
              }
            />
          </div>
        )}
      </div>
      {msg && <Alert>{msg}</Alert>}
      <Tabs<Tab>
        value={(["overview", "runs", "tasks", "logs", "bundle", "triggers"].includes(tab) ? tab : "overview") as Tab}
        onChange={setTab}
        tabs={[
          ["overview", "Overview"],
          ["runs", "Runs"],
          ["tasks", "Tasks"],
          ["logs", "Logs"],
          ["bundle", "Bundle"],
          ["triggers", `Triggers${d.triggers?.length ? ` (${d.triggers.length})` : ""}`],
        ]}
      />
      {tab === "runs" ? (
        <RunsTable filter={{ namespace, deployment: name }} />
      ) : tab === "tasks" ? (
        <TasksTable namespace={namespace} deployment={name} onOpen={setOpenTask} />
      ) : tab === "logs" ? (
        <Logs namespace={namespace} name={name} />
      ) : tab === "bundle" ? (
        <BundleFiles digest={d.definitionDigest} />
      ) : tab === "triggers" ? (
        <Triggers d={d} />
      ) : (
        <OverviewTab d={d} instances={dep.data?.instances ?? []} onOpenTask={setOpenTask} />
      )}
      {running && <RunTaskPanel d={d} onClose={() => setRunning(false)} />}
      {openTask && <TaskPanel id={openTask} onClose={() => setOpenTask(undefined)} />}
    </div>
  );
}

function OverviewTab({ d, instances, onOpenTask }: { d: Deployment; instances: Instance[]; onOpenTask: (id: string) => void }) {
  const metrics = useQuery(() => call<Metrics>("GetMetrics", { namespace: d.namespace, windowSeconds: 86400, buckets: 24 }), [d.namespace], {
    on: (e) => e.kind === "run" && e.deployment === d.name,
    every: 60_000,
  });
  const queued = useQuery(
    () => call<{ tasks?: Task[] }>("ListTasks", { namespace: d.namespace, deployment: d.name, state: "TASK_STATE_QUEUED", limit: 1000 }),
    [d.namespace, d.name],
    { on: (e) => e.kind === "task" && e.deployment === d.name },
  );
  const m = metrics.data?.deployments?.find((x) => x.ref.name === d.name);
  const l = d.limits ?? {};
  return (
    <div className="split">
      <div className="page" style={{ gap: 24 }}>
        <section className="section" aria-labelledby="last24">
          <div className="section-head">
            <h2 id="last24">Last 24 hours</h2>
            <a className="more" href={deploymentHref(d.namespace, d.name, "runs")}>
              All runs
            </a>
          </div>
          <dl className="kv">
            <dt>Runs</dt>
            <dd className="num" style={{ textAlign: "left" }}>
              {compact(m?.runs)} {m?.active ? <span className="muted">({m.active} active)</span> : null}
            </dd>
            <dt>Failed</dt>
            <dd>
              {m?.failed ? (
                <a href={`#/runs?deployment=${encodeURIComponent(d.name)}&status=failed&all=1`}>
                  {m.failed} ({pct(m.failed, m.runs ?? 0)})
                </a>
              ) : (
                "none"
              )}
            </dd>
            <dt>Duration</dt>
            <dd>{m && num(m.p50Ms) ? `median ${duration(num(m.p50Ms))}, p95 ${duration(num(m.p95Ms))}` : "–"}</dd>
            <dt>Tokens</dt>
            <dd>
              {compact(m?.usage?.inputTokens)} in · {compact(m?.usage?.outputTokens)} out
            </dd>
            <dt>Cost</dt>
            <dd>{money(m?.usage?.costUsd)}</dd>
          </dl>
        </section>
        <section className="section" aria-labelledby="limits">
          <div className="section-head">
            <h2 id="limits">Limits</h2>
            <span className="hint">Where this deployment stands against what its bundle allows.</span>
          </div>
          <Meter label="Spend today" value={d.spentUsdToday ?? 0} max={d.budget?.maxUsdPerDay} format={money} />
          <Meter label="Queued tasks" value={queued.data?.tasks?.length ?? 0} max={l.maxQueuedTasks} />
          <Meter label="Instances" value={d.desired ?? 0} max={d.scale?.max ?? 1} neutral />
          <dl className="kv">
            <dt>Per run</dt>
            <dd>
              {d.budget?.maxTokensPerRun ? `${compact(d.budget.maxTokensPerRun)} tokens` : "no token limit"}
              {d.budget?.maxUsdPerRun ? `, ${money(d.budget.maxUsdPerRun)}` : ""}
            </dd>
            <dt>Delegation</dt>
            <dd>
              depth {l.maxDelegationDepth || "∞"} · {l.maxFanOut || "∞"} calls per run · {l.maxTotalDelegations || "∞"} per tree
            </dd>
          </dl>
        </section>
        <section className="section" aria-labelledby="recent">
          <div className="section-head">
            <h2 id="recent">Recent tasks</h2>
            <a className="more" href={deploymentHref(d.namespace, d.name, "tasks")}>
              All tasks
            </a>
          </div>
          <TasksTable namespace={d.namespace} deployment={d.name} limit={5} onOpen={onOpenTask} />
        </section>
      </div>
      <section className="section" aria-labelledby="instances">
        <div className="section-head">
          <h2 id="instances">Instances</h2>
        </div>
        {instances.length === 0 ? (
          <Empty title={d.paused ? "Stopped" : "Asleep"}>
            {d.paused ? "Start the deployment to run instances." : "An instance starts when a task arrives or when you scale up."}
          </Empty>
        ) : (
          <ul className="list">
            {instances.map((i) => (
              <li key={i.id} style={{ flexDirection: "column", alignItems: "stretch", gap: 8 }} data-instance={i.deployment}>
                <div className="toolbar">
                  <Status spec={instanceStatus(i.state)} />
                  <span className="muted">
                    {i.runningTasks ? `${i.runningTasks} running · ` : ""}on {i.nestId.slice(0, 10)}
                  </span>
                  <span className="grow" />
                  <Time t={i.startedAt} />
                </div>
                {i.message && <span className="muted">{i.message}</span>}
                {(i.toolServers ?? []).length > 0 && (
                  <div className="toolbar">
                    {i.toolServers!.map((s) => (
                      <Badge key={s.name} tone={s.state === "connected" ? undefined : "err"} title={`${s.name}: ${s.state}`}>
                        {s.name} · {s.toolCount ?? 0} tools{s.state === "connected" ? "" : ` · ${s.state}`}
                      </Badge>
                    ))}
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

/** Submits a task, then follows it in the same panel. */
export function RunTaskPanel({ d, onClose }: { d: Deployment; onClose: () => void }) {
  const [input, setInput] = useState("");
  const [conversation, setConversation] = useState("");
  const [labels, setLabels] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [submitted, setSubmitted] = useState<string>();
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      const lab: Record<string, string> = {};
      for (const part of labels.split(",").map((s) => s.trim()).filter(Boolean)) {
        const [k, ...v] = part.split("=");
        if (!k || !v.length) throw new Error(`label ${part}: want key=value`);
        lab[k.trim()] = v.join("=").trim();
      }
      const r = await call<{ task: Task }>("SubmitTask", {
        ref: { namespace: d.namespace, name: d.name },
        input,
        conversationKey: conversation || undefined,
        labels: Object.keys(lab).length ? lab : undefined,
      });
      setError("");
      setSubmitted(r.task.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  }
  if (submitted)
    return (
      <Panel
        title="Task"
        onClose={onClose}
        footer={
          <button
            className="btn"
            onClick={() => {
              setSubmitted(undefined);
              setInput("");
            }}
          >
            Run another
          </button>
        }
      >
        <TaskDetails id={submitted} />
      </Panel>
    );
  return (
    <form onSubmit={submit}>
      <PanelForm title={`Run a task on ${d.name}`} onClose={onClose} busy={busy} disabled={!input.trim()} submit="Submit task">
        <label className="field">
          Task input
          <textarea rows={6} value={input} onChange={(e) => setInput(e.target.value)} autoFocus aria-label="Task input" />
        </label>
        <label className="field">
          Conversation key <span className="help">Optional. Tasks with the same key continue one conversation.</span>
          <input value={conversation} onChange={(e) => setConversation(e.target.value)} placeholder="e.g. ticket-4211" />
        </label>
        <label className="field">
          Labels <span className="help">Optional, comma separated key=value. They follow the task to its runs and tool calls.</span>
          <input value={labels} onChange={(e) => setLabels(e.target.value)} placeholder="team=support, customer=acme" />
        </label>
        {error && <Alert>{error}</Alert>}
      </PanelForm>
    </form>
  );
}

/** A side panel inside a form: its footer submits the form. */
export function PanelForm({
  title,
  onClose,
  children,
  submit,
  busy,
  disabled,
  wide,
}: {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
  submit: string;
  busy?: boolean;
  disabled?: boolean;
  wide?: boolean;
}) {
  return (
    <Panel
      title={title}
      onClose={onClose}
      wide={wide}
      footer={
        <>
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn primary" disabled={busy || disabled}>
            {submit}
          </button>
        </>
      }
    >
      {children}
    </Panel>
  );
}

function Logs({ namespace, name }: { namespace: string; name: string }) {
  const [level, setLevel] = useState("all");
  const logs = useQuery(() => call<{ lines?: LogLine[] }>("GetLogs", { ref: { namespace, name }, limit: 300 }), [namespace, name], {
    on: (e) => e.deployment === name && e.namespace === namespace,
    every: 10_000,
  });
  const box = useRef<HTMLPreElement>(null);
  const lines = (logs.data?.lines ?? []).filter((l) => level === "all" || l.level === level);
  useEffect(() => {
    const el = box.current;
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight < 80) el.scrollTop = el.scrollHeight;
  }, [logs.data]);
  return (
    <div className="section">
      <div className="toolbar">
        <select value={level} onChange={(e) => setLevel(e.target.value)} aria-label="Log level">
          <option value="all">All levels</option>
          <option value="info">Info</option>
          <option value="warn">Warnings</option>
          <option value="error">Errors</option>
        </select>
        <span className="muted">Newest at the bottom; follows while you are at the end.</span>
      </div>
      {logs.error && <Alert>{logs.error}</Alert>}
      {lines.length === 0 && !logs.loading ? (
        <Empty title="No log lines">Runs, instances and tasks write here as they happen.</Empty>
      ) : (
        <pre className="code" ref={box} style={{ maxHeight: "60vh" }} aria-label="Logs">
          {lines.map((l, i) => (
            <span key={i} style={{ color: l.level === "error" ? "var(--err)" : l.level === "warn" ? "var(--warn)" : undefined }}>
              {new Date(l.time).toLocaleTimeString()} {l.instanceId ? l.instanceId.slice(0, 8) : "hub     "} {l.level.padEnd(5)} {l.message}
              {"\n"}
            </span>
          ))}
        </pre>
      )}
    </div>
  );
}

function BundleFiles({ digest }: { digest: string }) {
  const def = useQuery(() => call<{ definition: Definition; textFiles?: Record<string, string> }>("GetDefinition", { digest }), [digest]);
  const files = useMemo(() => {
    const out: Record<string, string | undefined> = {};
    for (const [p, v] of Object.entries(def.data?.definition.files ?? {})) out[p] = def.data?.textFiles?.[p] ?? textOf(v);
    return out;
  }, [def.data]);
  const paths = Object.keys(files).sort();
  const [sel, setSel] = useState<string>();
  const current = sel ?? paths.find((p) => p === "x-agen/agent.md") ?? paths[0];
  if (def.error) return <Alert>{def.error}</Alert>;
  if (def.loading) return <SkeletonRows rows={3} cols={2} />;
  const text = current ? files[current] : undefined;
  return (
    <div className="split" style={{ gridTemplateColumns: "minmax(180px, 260px) minmax(0, 1fr)" }}>
      <ul className="list" aria-label="Bundle files">
        {paths.map((p) => (
          <li key={p} style={{ padding: 0 }}>
            <button
              className="btn ghost"
              style={{ width: "100%", justifyContent: "flex-start", borderRadius: 0, height: 34 }}
              aria-current={p === current || undefined}
              onClick={() => setSel(p)}
            >
              <FileCode2 size={14} aria-hidden />
              <span className="truncate">{p}</span>
            </button>
          </li>
        ))}
      </ul>
      <div className="section">
        <div className="toolbar">
          <span className="mono">{current}</span>
        </div>
        {text === undefined ? (
          <Empty title="Binary file">It is part of the bundle but not shown here.</Empty>
        ) : current?.endsWith(".json") ? (
          <Json value={text} label={`content of ${current}`} />
        ) : (
          <pre className="code" style={{ maxHeight: "65vh" }} aria-label={`content of ${current}`}>
            {text}
          </pre>
        )}
      </div>
    </div>
  );
}

function Triggers({ d }: { d: Deployment }) {
  const events = useQuery(() => call<{ events?: TriggerEvent[] }>("ListTriggerEvents", { ref: { namespace: d.namespace, name: d.name }, limit: 50 }), [d.namespace, d.name], {
    on: (e) => e.kind === "task" && e.deployment === d.name,
  });
  if (!d.triggers?.length) return <Empty title="No triggers">Add cron or webhook triggers in x-agen/config.json to start tasks on a schedule or from other systems.</Empty>;
  return (
    <div className="page" style={{ gap: 16 }}>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Trigger</th>
              <th>Type</th>
              <th>When</th>
            </tr>
          </thead>
          <tbody>
            {d.triggers.map((t) => (
              <tr key={t.name}>
                <td>{t.name}</td>
                <td>{t.type}</td>
                <td className="mono">{t.type === "cron" ? t.schedule : `POST /hooks/${d.namespace}/${d.name}/${t.name}`}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <h2>Recent firings</h2>
      {(events.data?.events ?? []).length === 0 ? (
        <p className="muted">None yet.</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Trigger</th>
                <th>State</th>
                <th>Due</th>
                <th>Task</th>
                <th>Message</th>
              </tr>
            </thead>
            <tbody>
              {events.data!.events!.map((e, i) => (
                <tr key={i}>
                  <td>{e.trigger}</td>
                  <td>
                    <Badge tone={e.state.endsWith("FIRED") ? undefined : "warn"}>{enumName(e.state, "TRIGGER_EVENT_STATE_")}</Badge>
                  </td>
                  <td>
                    <At t={e.dueAt} />
                  </td>
                  <td className="mono">{e.taskId?.slice(0, 12)}</td>
                  <td>{e.message}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}


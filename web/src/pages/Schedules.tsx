import { AlertTriangle, Play, X } from "lucide-react";
import { useState } from "react";
import { call, Deployment, Task, TriggerEvent } from "../api";
import { useSession } from "../App";
import { errorText } from "../api";
import { useQuery } from "../live";
import { deploymentHref } from "../router";
import { Alert, Badge, Empty, SkeletonRows, Time } from "../ui";
import { TaskPanel } from "./Tasks";

/** A cron expression in words, for the common shapes; otherwise as written. */
export function describeCron(expr: string): string {
  const e = expr.trim();
  const every = e.match(/^@every\s+(\S+)$/);
  if (every) return `every ${every[1]}`;
  const named: Record<string, string> = { "@hourly": "every hour", "@daily": "every day at 00:00", "@weekly": "every Sunday at 00:00", "@monthly": "on the 1st at 00:00" };
  if (named[e]) return named[e];
  const tz = e.match(/^CRON_TZ=(\S+)\s+(.*)$/);
  const body = tz ? tz[2] : e;
  const zone = tz ? tz[1] : "UTC";
  const f = body.split(/\s+/);
  if (f.length !== 5) return e;
  const [min, hour, dom, mon, dow] = f;
  const at = /^\d+$/.test(min) && /^\d+$/.test(hour) ? `${hour.padStart(2, "0")}:${min.padStart(2, "0")} ${zone}` : "";
  if (min.startsWith("*/") && hour === "*" && dom === "*" && mon === "*" && dow === "*") return `every ${min.slice(2)} minutes`;
  if (/^\d+$/.test(min) && hour === "*" && dom === "*" && mon === "*" && dow === "*") return `every hour at :${min.padStart(2, "0")}`;
  if (at && dom === "*" && mon === "*" && dow === "*") return `every day at ${at}`;
  if (at && dom === "*" && mon === "*" && dow === "1-5") return `weekdays at ${at}`;
  if (at && dom === "*" && mon === "*" && /^\d$/.test(dow)) return `every ${["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"][+dow % 7]} at ${at}`;
  return `${body} (${zone})`;
}

/**
 * Everything that runs on its own or later: recurring schedules (with their
 * next run), tasks waiting for a start time, webhooks, and the trigger
 * events that went wrong.
 */
export function Schedules() {
  const { can } = useSession();
  const deps = useQuery(() => call<{ deployments?: Deployment[] }>("ListDeployments", {}), [], { on: ["deployment", "task"], every: 60_000 });
  const upcoming = useQuery(() => call<{ tasks?: Task[] }>("ListTasks", { scheduled: true, limit: 200 }), [], { on: ["task"] });
  const events = useQuery(() => call<{ events?: TriggerEvent[] }>("ListTriggerEvents", { limit: 200 }), [], { on: ["task"], every: 60_000 });
  const [openTask, setOpenTask] = useState<string>();
  const [msg, setMsg] = useState("");
  const [secret, setSecret] = useState<{ path: string; secret: string }>();

  const list = deps.data?.deployments ?? [];
  const crons = list.flatMap((d) => (d.triggers ?? []).filter((t) => t.type === "cron").map((t) => ({ d, t })));
  crons.sort((a, b) => Date.parse(a.t.nextDueAt ?? "") - Date.parse(b.t.nextDueAt ?? ""));
  const hooks = list.flatMap((d) => (d.triggers ?? []).filter((t) => t.type === "webhook").map((t) => ({ d, t })));
  const all = events.data?.events ?? [];
  const lastFired = new Map<string, TriggerEvent>();
  for (const e of all) if (e.state.endsWith("FIRED") && !lastFired.has(`${e.deployment}/${e.trigger}`)) lastFired.set(`${e.deployment}/${e.trigger}`, e);
  const problems = all.filter((e) => e.state.endsWith("MISSED") || e.state.endsWith("REJECTED"));
  const misses = (d: string, t: string) => problems.filter((e) => e.deployment === d && e.trigger === t && e.state.endsWith("MISSED")).length;

  const act = async (f: () => Promise<unknown>, done: string) => {
    try {
      await f();
      setMsg(done);
    } catch (e) {
      setMsg(errorText(e));
    }
  };

  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Schedules</h1>
          <p className="facts">
            <span>
              <b>{crons.length}</b> recurring
            </span>
            <span>
              <b>{upcoming.data?.tasks?.length ?? 0}</b> one-off tasks waiting
            </span>
            <span>
              <b>{hooks.length}</b> webhooks
            </span>
            {problems.length > 0 && (
              <span>
                <b>{problems.length}</b> missed or refused
              </span>
            )}
          </p>
        </div>
      </div>
      {msg && <Alert tone="info">{msg}</Alert>}

      <section className="section" aria-labelledby="recurring">
        <h2 id="recurring">Recurring</h2>
        {deps.loading ? (
          <SkeletonRows rows={3} cols={6} />
        ) : !crons.length ? (
          <Empty title="No schedules">Add a cron trigger to a deployment's x-agen/config.json to run it on a schedule.</Empty>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Deployment</th>
                  <th>Schedule</th>
                  <th>Next run</th>
                  <th>Last run</th>
                  <th>Catch up</th>
                  <th className="num">Missed (recent)</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {crons.map(({ d, t }) => {
                  const last = lastFired.get(`${d.name}/${t.name}`);
                  const blocked = d.paused ? "stopped" : d.budgetExhausted ? "daily budget used" : d.providerHold ? "provider held" : "";
                  return (
                    <tr key={`${d.namespace}/${d.name}/${t.name}`}>
                      <td>
                        <a href={deploymentHref(d.namespace, d.name, "triggers")}>{d.name}</a>
                        <span className="muted"> · {t.name}</span>
                      </td>
                      <td title={t.schedule}>
                        {describeCron(t.schedule ?? "")}
                        <div className="muted small">
                          <code>{t.schedule}</code>
                        </div>
                      </td>
                      <td>
                        {blocked ? <Badge tone="warn">{blocked}</Badge> : <Time t={t.nextDueAt} />}
                        <div className="muted small">{t.nextDueAt ? new Date(t.nextDueAt).toLocaleString() : ""}</div>
                      </td>
                      <td>
                        {last ? (
                          <button className="link" onClick={() => last.taskId && setOpenTask(last.taskId)}>
                            <Time t={last.dueAt} />
                          </button>
                        ) : (
                          <span className="muted">not yet</span>
                        )}
                      </td>
                      <td>{t.catchUp ? "yes" : "no"}</td>
                      <td className="num" data-tone={misses(d.name, t.name) ? "warn" : undefined}>
                        {misses(d.name, t.name)}
                      </td>
                      <td className="actions">
                        {can("operator") && (
                          <button
                            className="btn sm"
                            title="Submit the trigger's task now"
                            onClick={() =>
                              act(
                                () => call("SubmitTask", { ref: { namespace: d.namespace, name: d.name }, input: t.input ?? "", labels: t.labels }),
                                `Submitted ${d.name}'s "${t.name}" task.`,
                              )
                            }
                          >
                            <Play size={13} aria-hidden /> Run now
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="section" aria-labelledby="upcoming">
        <h2 id="upcoming">One-off tasks waiting for their start time</h2>
        {upcoming.error && <Alert>{upcoming.error}</Alert>}
        {upcoming.loading ? (
          <SkeletonRows rows={2} cols={5} />
        ) : !(upcoming.data?.tasks ?? []).length ? (
          <Empty title="Nothing scheduled">Tasks submitted with a start time (agen run --at / --in, or "Start at" when running a task) wait here.</Empty>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Starts</th>
                  <th>Deployment</th>
                  <th>Input</th>
                  <th>Source</th>
                  <th>Tier</th>
                  <th>Submitted</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(upcoming.data?.tasks ?? []).map((t) => (
                  <tr key={t.id}>
                    <td>
                      <Time t={t.notBefore} />
                      <div className="muted small">{t.notBefore ? new Date(t.notBefore).toLocaleString() : ""}</div>
                    </td>
                    <td>
                      <a href={deploymentHref(t.namespace, t.deployment)}>{t.deployment}</a>
                    </td>
                    <td className="truncate" style={{ maxWidth: 320 }}>
                      <button className="link" onClick={() => setOpenTask(t.id)} title={t.input}>
                        {t.input?.split("\n")[0] || t.id}
                      </button>
                    </td>
                    <td>{t.source}</td>
                    <td>{t.modelTier || <span className="muted">–</span>}</td>
                    <td>
                      <Time t={t.createdAt} />
                    </td>
                    <td className="actions">
                      {can("operator") && (
                        <button className="btn sm" onClick={() => act(() => call("CancelTask", { id: t.id }).then(() => upcoming.reload()), "Cancelled.")}>
                          <X size={13} aria-hidden /> Cancel
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="section" aria-labelledby="webhooks">
        <h2 id="webhooks">Webhooks</h2>
        {secret && (
          <Alert tone="info">
            New secret for <code>{secret.path}</code>, shown once: <code>{secret.secret}</code>
          </Alert>
        )}
        {!hooks.length ? (
          <Empty title="No webhooks">Add a webhook trigger to a deployment to let other systems start its work.</Empty>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Deployment</th>
                  <th>Path</th>
                  <th>Auth</th>
                  <th>Last delivery</th>
                  <th className="num">Refused (recent)</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {hooks.map(({ d, t }) => {
                  const path = `/hooks/${d.namespace}/${d.name}/${t.name}`;
                  const refused = problems.filter((e) => e.deployment === d.name && e.trigger === t.name).length;
                  const last = lastFired.get(`${d.name}/${t.name}`);
                  return (
                    <tr key={path}>
                      <td>
                        <a href={deploymentHref(d.namespace, d.name, "triggers")}>{d.name}</a>
                        <span className="muted"> · {t.name}</span>
                      </td>
                      <td>
                        <code>{path}</code>
                      </td>
                      <td>{t.auth?.type === "hmac" ? `HMAC (${t.auth.header ?? "signature header"})` : "bearer token"}</td>
                      <td>{last ? <Time t={last.dueAt} /> : <span className="muted">none yet</span>}</td>
                      <td className="num" data-tone={refused ? "warn" : undefined}>
                        {refused}
                      </td>
                      <td className="actions">
                        {can("operator") && (!t.auth || t.auth.type !== "hmac") && (
                          <button
                            className="btn sm"
                            title="Create or rotate the secret callers present (the old one stops working)"
                            onClick={() =>
                              act(async () => {
                                const r = await call<{ secret: string }>("CreateWebhookSecret", { ref: { namespace: d.namespace, name: d.name }, trigger: t.name });
                                setSecret({ path, secret: r.secret });
                              }, "")
                            }
                          >
                            New secret
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="section" aria-labelledby="problems">
        <h2 id="problems">Missed and refused</h2>
        {!problems.length ? (
          <p className="muted">No missed windows or refused webhook calls.</p>
        ) : (
          <ul className="list">
            {problems.slice(0, 50).map((e, i) => (
              <li key={`${e.deployment}/${e.trigger}/${e.dueAt}/${i}`}>
                <AlertTriangle size={16} style={{ color: "var(--warn)" }} aria-hidden />
                <span className="grow" style={{ flex: 1, minWidth: 0 }}>
                  <b>{e.deployment}</b> · {e.trigger}: {e.state.endsWith("MISSED") ? "missed" : "refused"}
                  {e.message ? <span className="muted"> — {e.message}</span> : null}
                </span>
                <Time t={e.dueAt} />
              </li>
            ))}
          </ul>
        )}
      </section>
      {openTask && <TaskPanel id={openTask} onClose={() => setOpenTask(undefined)} />}
    </div>
  );
}

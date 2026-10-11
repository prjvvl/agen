import { call, Instance, Nest } from "../api";
import { enumName } from "../format";
import { useQuery } from "../live";
import { deploymentHref } from "../router";
import { Alert, Badge, Empty, instanceStatus, Meter, SkeletonRows, Status, Time } from "../ui";

export function Fleet() {
  const nests = useQuery(() => call<{ nests?: Nest[] }>("ListNests"), [], { on: ["instance"], every: 15_000 });
  const instances = useQuery(() => call<{ instances?: Instance[] }>("ListInstances", {}), [], { on: ["instance"], every: 30_000 });
  const byNest = new Map<string, Instance[]>();
  for (const i of instances.data?.instances ?? []) (byNest.get(i.nestId) ?? byNest.set(i.nestId, []).get(i.nestId)!).push(i);
  const list = nests.data?.nests ?? [];
  return (
    <div className="page">
      <div className="page-head">
        <div className="titles">
          <h1>Fleet</h1>
          <p className="facts">Nests are the machines and clusters that run instances. Add one with agen join-token and agen nest run.</p>
        </div>
      </div>
      {(nests.error || instances.error) && <Alert>{nests.error || instances.error}</Alert>}
      {nests.loading ? (
        <SkeletonRows rows={2} cols={4} />
      ) : list.length === 0 ? (
        <Empty title="No nests enrolled">Run agen up for a local one, or enrol a machine with agen nest run --hub URL --join-token TOKEN.</Empty>
      ) : (
        list.map((n) => {
          const inst = byNest.get(n.id) ?? [];
          const state = enumName(n.state, "NEST_STATE_");
          return (
            <section className="section" key={n.id} aria-label={`Nest ${n.name}`}>
              <div className="section-head">
                <h2>{n.name}</h2>
                <Badge tone={state === "active" ? "ok" : state === "lost" || state === "revoked" ? "err" : "warn"}>{state}</Badge>
                <span className="hint">
                  {n.backend} · heartbeat <Time t={n.lastHeartbeat} />
                </span>
              </div>
              {(n.capacity ?? 0) > 0 && <Meter label="Capacity" value={n.used ?? 0} max={n.capacity} />}
              {inst.length === 0 ? (
                <p className="muted">No instances running here.</p>
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>Deployment</th>
                        <th>State</th>
                        <th className="num">Tasks</th>
                        <th>Tools</th>
                        <th>Started</th>
                        <th>Instance</th>
                      </tr>
                    </thead>
                    <tbody>
                      {inst.map((i) => (
                        <tr key={i.id} data-instance={i.deployment}>
                          <td>
                            <a href={deploymentHref(i.namespace, i.deployment)}>
                              {i.namespace}/{i.deployment}
                            </a>
                          </td>
                          <td>
                            <Status spec={instanceStatus(i.state)} />
                          </td>
                          <td className="num">{i.runningTasks ?? 0}</td>
                          <td>
                            <span className="toolbar">
                              {(i.toolServers ?? []).map((s) => (
                                <Badge key={s.name} tone={s.state === "connected" ? undefined : "err"}>
                                  {s.name}
                                </Badge>
                              ))}
                              {!(i.toolServers ?? []).length && <span className="muted">{(i.tools ?? []).length} tools</span>}
                            </span>
                          </td>
                          <td>
                            <Time t={i.startedAt} />
                          </td>
                          <td className="mono muted">{i.id.slice(0, 12)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
          );
        })
      )}
    </div>
  );
}

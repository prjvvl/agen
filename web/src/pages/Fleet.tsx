import { call, Instance, lower, Nest, short } from "../api";
import { usePoll } from "../hooks";

export function Fleet() {
  const nests = usePoll(() => call<{ nests?: Nest[] }>("ListNests", {}), []);
  const insts = usePoll(() => call<{ instances?: Instance[] }>("ListInstances", {}), []);
  const nestName = Object.fromEntries((nests.data?.nests ?? []).map((n) => [n.id, n.name]));
  return (
    <section>
      <h2>Fleet</h2>
      {(nests.error || insts.error) && <p className="error">{nests.error || insts.error}</p>}
      <h3>Nests</h3>
      <table>
        <thead>
          <tr>
            <th>Nest</th>
            <th>Backend</th>
            <th>State</th>
            <th>Used / capacity</th>
            <th>Last heartbeat</th>
          </tr>
        </thead>
        <tbody>
          {(nests.data?.nests ?? []).map((n) => (
            <tr key={n.id}>
              <td>{n.name}</td>
              <td>{n.backend}</td>
              <td>{lower(n.state, "NEST_STATE_")}</td>
              <td>
                {n.used ?? 0} / {n.capacity || "∞"}
              </td>
              <td>{n.lastHeartbeat ? new Date(n.lastHeartbeat).toLocaleTimeString() : ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <h3>Instances</h3>
      {(insts.data?.instances ?? []).length === 0 ? (
        <p className="empty">No instances running (deployments are asleep or stopped).</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Instance</th>
              <th>Deployment</th>
              <th>Nest</th>
              <th>State</th>
              <th>Tasks</th>
              <th>Definition</th>
            </tr>
          </thead>
          <tbody>
            {insts.data!.instances!.map((i) => (
              <tr key={i.id} data-instance={i.deployment}>
                <td className="mono">{short(i.id, 26)}</td>
                <td>
                  {i.namespace}/{i.deployment}
                </td>
                <td>{nestName[i.nestId] ?? short(i.nestId)}</td>
                <td>{lower(i.state, "INSTANCE_STATE_")}</td>
                <td>{i.runningTasks ?? 0}</td>
                <td className="mono">{short(i.definitionDigest)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

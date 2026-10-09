import { useState } from "react";
import { call, Deployment, lower, short } from "../api";
import { errorText, usePoll } from "../hooks";

export function status(d: Deployment): string {
  if (d.paused) return "stopped";
  if (d.budgetExhausted) return `daily budget used ($${(d.spentUsdToday ?? 0).toFixed(2)})`;
  return (d.ready ?? 0) > 0 ? "running" : "asleep";
}

export function Deployments() {
  const { data, error, reload } = usePoll(() => call<{ deployments?: Deployment[] }>("ListDeployments", {}), []);
  const [actionError, setActionError] = useState("");
  const [pending, setPending] = useState(false);
  // One action at a time: a double click must not send the same step twice.
  const act = async (f: () => Promise<unknown>) => {
    if (pending) return;
    setPending(true);
    try {
      await f();
      setActionError("");
    } catch (e) {
      setActionError(errorText(e));
    }
    await reload();
    setPending(false);
  };
  const deps = data?.deployments ?? [];
  return (
    <section>
      <h2>Deployments</h2>
      {(error || actionError) && <p className="error">{error || actionError}</p>}
      {deps.length === 0 ? (
        <p className="empty">
          No deployments yet. <a href="#/new">Create an agent</a> or run <code>agen deploy</code>.
        </p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Deployment</th>
              <th>Kind</th>
              <th>Ready</th>
              <th>Desired</th>
              <th>Scale</th>
              <th>Definition</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {deps.map((d) => {
              const ref = { namespace: d.namespace, name: d.name };
              const min = d.scale?.min ?? 0;
              const max = d.scale?.max ?? 1;
              const desired = d.desired ?? 0;
              const scale = (n: number) => act(() => call("ScaleDeployment", { ref, desired: n }));
              return (
                <tr key={d.namespace + "/" + d.name} data-deployment={d.name}>
                  <td>
                    <a href={`#/deployments/${d.namespace}/${d.name}`}>
                      {d.namespace}/{d.name}
                    </a>
                  </td>
                  <td>{lower(d.kind, "DEPLOYMENT_KIND_")}</td>
                  <td data-field="ready">{d.ready ?? 0}</td>
                  <td>
                    <div className="stepper">
                      <button aria-label={`scale ${d.name} down`} disabled={pending || d.paused || desired <= min} onClick={() => scale(desired - 1)}>
                        −
                      </button>
                      <span data-field="desired">{desired}</span>
                      <button aria-label={`scale ${d.name} up`} disabled={pending || d.paused || desired >= max} onClick={() => scale(desired + 1)}>
                        +
                      </button>
                    </div>
                  </td>
                  <td>
                    {min}..{max}
                  </td>
                  <td className="mono">{short(d.definitionDigest)}</td>
                  <td data-field="status">
                    {status(d)}{" "}
                    <button className="small" onClick={() => act(() => call("PauseDeployment", { ref, paused: !d.paused }))}>
                      {d.paused ? "Start" : "Stop"}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </section>
  );
}

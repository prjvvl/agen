import { useState } from "react";
import { Approval, call, lower } from "../api";
import { errorText, usePoll } from "../hooks";

export function Approvals() {
  const [all, setAll] = useState(false);
  const { data, error, reload } = usePoll(
    () => call<{ approvals?: Approval[] }>("ListApprovals", all ? {} : { state: "APPROVAL_STATE_PENDING" }),
    [all],
  );
  const [msg, setMsg] = useState("");
  async function decide(id: string, approve: boolean) {
    try {
      await call("DecideApproval", { id, approve });
      setMsg("");
    } catch (e) {
      setMsg(errorText(e));
    }
    reload();
  }
  const list = data?.approvals ?? [];
  return (
    <section>
      <h2>Approvals</h2>
      <label className="inline">
        <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} /> Show decided
      </label>
      {(error || msg) && <p className="error">{error || msg}</p>}
      {list.length === 0 ? (
        <p className="empty">No {all ? "" : "pending "}approvals.</p>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Deployment</th>
              <th>Tool</th>
              <th>Arguments</th>
              <th>Requested by</th>
              <th>State</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {list.map((a) => (
              <tr key={a.id} data-approval={a.id}>
                <td>
                  {a.namespace}/{a.deployment}
                </td>
                <td>{a.tool}</td>
                <td className="mono">{JSON.stringify(a.arguments ?? {})}</td>
                <td>{a.requestedBy}</td>
                <td data-field="state">{lower(a.state, "APPROVAL_STATE_")}</td>
                <td>
                  {a.state === "APPROVAL_STATE_PENDING" && (
                    <>
                      <button className="small" onClick={() => decide(a.id, true)}>
                        Approve
                      </button>{" "}
                      <button className="small danger" onClick={() => decide(a.id, false)}>
                        Deny
                      </button>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

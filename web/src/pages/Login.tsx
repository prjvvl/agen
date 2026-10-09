import { useState } from "react";
import { call, setToken } from "../api";
import { errorText } from "../hooks";

export function Login({ onLogin }: { onLogin: () => void }) {
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setToken(value.trim());
    try {
      await call("ListDeployments", {});
      onLogin();
    } catch (err) {
      setToken("");
      setError(errorText(err));
    }
  }
  return (
    <div className="login">
      <h1>agen</h1>
      <form onSubmit={submit}>
        <label>
          API token
          <input type="password" value={value} onChange={(e) => setValue(e.target.value)} autoFocus aria-label="API token" />
        </label>
        <button type="submit">Sign in</button>
        {error && <p className="error">{error}</p>}
      </form>
      <p className="hint">
        Run <code>agen ui</code> for a sign-in link, or create a token with <code>agen token create</code>.
      </p>
    </div>
  );
}

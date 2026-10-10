import { useState } from "react";
import { call, setToken } from "../api";
import { Alert } from "../ui";

export function Login({ onLogin }: { onLogin: () => void }) {
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setToken(value.trim());
    try {
      await call("WhoAmI", {});
      onLogin();
    } catch (err) {
      setToken("");
      setError(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  }
  return (
    <div className="login">
      <form onSubmit={submit}>
        <div className="brand">
          <span className="brand-mark" aria-hidden>
            <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round">
              <path d="M3 13 8 3l5 10M5.2 9h5.6" />
            </svg>
          </span>
          agen
        </div>
        <h1>Sign in to the console</h1>
        <label className="field">
          API token
          <input type="password" value={value} onChange={(e) => setValue(e.target.value)} autoFocus autoComplete="off" aria-label="API token" />
        </label>
        <button className="btn primary" type="submit" disabled={busy || !value.trim()}>
          Sign in
        </button>
        {error && <Alert>{error}</Alert>}
        <p className="hint">
          Run <code>agen ui</code> for a sign-in link, or create a token with <code>agen token create</code>.
        </p>
      </form>
    </div>
  );
}

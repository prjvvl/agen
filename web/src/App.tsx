import { Component, ReactNode, useEffect, useState } from "react";
import { setToken, token, tokenFromFragment } from "./api";
import { Approvals } from "./pages/Approvals";
import { BundleEditor } from "./pages/BundleEditor";
import { DeploymentDetail } from "./pages/DeploymentDetail";
import { Deployments } from "./pages/Deployments";
import { Fleet } from "./pages/Fleet";
import { Login } from "./pages/Login";
import { TraceView } from "./pages/TraceView";

// Hash routes: #/deployments, #/deployments/<ns>/<name>, #/new,
// #/edit/<ns>/<name>, #/fleet, #/approvals, #/trace/<traceId>.
function useRoute(): string[] {
  const decode = (s: string) => {
    try {
      return decodeURIComponent(s);
    } catch {
      return s; // malformed escape: keep it as typed
    }
  };
  const parse = () => (location.hash.replace(/^#\/?/, "") || "deployments").split("/").map(decode);
  const [route, setRoute] = useState(parse);
  useEffect(() => {
    const on = () => setRoute(parse());
    addEventListener("hashchange", on);
    return () => removeEventListener("hashchange", on);
  }, []);
  return route;
}

export function App() {
  const [authed, setAuthed] = useState(!!token());
  const route = useRoute();
  // A sign-in link opened in a tab that already runs the UI.
  useEffect(() => {
    const on = () => {
      if (location.hash.includes("token=")) {
        tokenFromFragment();
        setAuthed(!!token());
      }
    };
    const out = () => setAuthed(false); // the API said 401
    addEventListener("hashchange", on);
    addEventListener("agen:signed-out", out);
    return () => {
      removeEventListener("hashchange", on);
      removeEventListener("agen:signed-out", out);
    };
  }, []);
  if (!authed) return <Login onLogin={() => setAuthed(true)} />;
  const [page, a, b] = route;
  const nav = [
    ["deployments", "Deployments"],
    ["fleet", "Fleet"],
    ["approvals", "Approvals"],
    ["new", "New agent"],
  ];
  let body;
  switch (page) {
    case "fleet":
      body = <Fleet />;
      break;
    case "approvals":
      body = <Approvals />;
      break;
    case "new":
      body = <BundleEditor />;
      break;
    case "edit":
      body = <BundleEditor namespace={a} name={b} />;
      break;
    case "trace":
      body = <TraceView traceId={a} />;
      break;
    case "deployments":
      body = a && b ? <DeploymentDetail namespace={a} name={b} /> : <Deployments />;
      break;
    default:
      body = <p>Not found.</p>;
  }
  return (
    <div className="app">
      <header>
        <a className="brand" href="#/deployments">
          agen
        </a>
        <nav>
          {nav.map(([id, label]) => (
            <a key={id} href={`#/${id}`} className={page === id ? "active" : ""}>
              {label}
            </a>
          ))}
        </nav>
        <button
          className="link"
          onClick={() => {
            setToken("");
            setAuthed(false);
          }}
        >
          Sign out
        </button>
      </header>
      <main>
        <Boundary key={route.join("/")}>{body}</Boundary>
      </main>
    </div>
  );
}

// A page that throws shows the error instead of a blank screen.
class Boundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {};
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  render() {
    return this.state.error ? <p className="error">Something went wrong: {this.state.error.message}</p> : this.props.children;
  }
}

import {
  Activity,
  BookOpen,
  Boxes,
  Coins,
  Inbox as InboxIcon,
  LayoutDashboard,
  LayoutTemplate,
  LogOut,
  Menu,
  MessageSquare,
  Monitor,
  Moon,
  Search,
  Server,
  Settings as SettingsIcon,
  Sun,
} from "lucide-react";
import { Component, createContext, ReactNode, useContext, useEffect, useState } from "react";
import { Approval, call, setToken, Task, token, tokenFromFragment, WhoAmI } from "./api";
import { useLiveState, useQuery } from "./live";
import { Assistant } from "./pages/Assistant";
import { BundleEditor } from "./pages/BundleEditor";
import { CommandPalette } from "./pages/CommandPalette";
import { Costs } from "./pages/Costs";
import { DeploymentDetail } from "./pages/DeploymentDetail";
import { Deployments } from "./pages/Deployments";
import { Fleet } from "./pages/Fleet";
import { Inbox } from "./pages/Inbox";
import { Login } from "./pages/Login";
import { Overview } from "./pages/Overview";
import { Runs } from "./pages/Runs";
import { Settings } from "./pages/Settings";
import { Templates } from "./pages/Templates";
import { TraceView } from "./pages/TraceView";
import { Route, useRoute } from "./router";
import { Alert } from "./ui";

export interface Session {
  me?: WhoAmI;
  can: (scope: "viewer" | "operator" | "approver" | "admin") => boolean;
  openAssistant: (prompt?: string) => void;
}

const SessionContext = createContext<Session>({ can: () => false, openAssistant: () => {} });
export const useSession = () => useContext(SessionContext);

function can(me: WhoAmI | undefined, scope: string): boolean {
  const s = me?.scopes ?? [];
  if (s.includes("admin")) return true;
  if (scope === "viewer") return s.length > 0;
  return s.includes(scope);
}

type Theme = "system" | "light" | "dark";

function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(() => {
    try {
      return (localStorage.getItem("agen.theme") as Theme) || "system";
    } catch {
      return "system";
    }
  });
  useEffect(() => {
    const root = document.documentElement;
    if (theme === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", theme);
    try {
      localStorage.setItem("agen.theme", theme);
    } catch {
      /* storage unavailable: the choice lasts for this page */
    }
  }, [theme]);
  const next = () => setTheme((t) => (t === "system" ? "dark" : t === "dark" ? "light" : "system"));
  return [theme, next];
}

export function App() {
  const [authed, setAuthed] = useState(!!token());
  useEffect(() => {
    // A sign-in link opened in a tab that already runs the UI.
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
  return <Console onSignOut={() => setAuthed(false)} />;
}

const NAV: { id: string; label: string; icon: typeof Activity; match: (r: Route) => boolean }[] = [
  { id: "", label: "Overview", icon: LayoutDashboard, match: (r) => r.path.length === 0 || r.path[0] === "overview" },
  { id: "inbox", label: "Inbox", icon: InboxIcon, match: (r) => r.path[0] === "inbox" },
  { id: "deployments", label: "Deployments", icon: Boxes, match: (r) => ["deployments", "new", "edit"].includes(r.path[0]) },
  { id: "runs", label: "Runs", icon: Activity, match: (r) => r.path[0] === "runs" || r.path[0] === "trace" },
  { id: "costs", label: "Costs", icon: Coins, match: (r) => r.path[0] === "costs" },
  { id: "templates", label: "Templates", icon: LayoutTemplate, match: (r) => r.path[0] === "templates" },
  { id: "fleet", label: "Fleet", icon: Server, match: (r) => r.path[0] === "fleet" },
  { id: "settings", label: "Settings", icon: SettingsIcon, match: (r) => r.path[0] === "settings" },
];

function Console({ onSignOut }: { onSignOut: () => void }) {
  const route = useRoute();
  const live = useLiveState();
  const [theme, nextTheme] = useTheme();
  const [menu, setMenu] = useState(false);
  const [palette, setPalette] = useState(false);
  const [assistant, setAssistant] = useState<{ open: boolean; prompt?: string }>({ open: false });
  const who = useQuery(() => call<WhoAmI>("WhoAmI"), []);
  const me = who.data;
  const inbox = useInboxCount(can(me, "approver"));

  useEffect(() => setMenu(false), [route]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    addEventListener("keydown", onKey);
    return () => removeEventListener("keydown", onKey);
  }, []);

  const session: Session = {
    me,
    can: (scope) => can(me, scope),
    openAssistant: (prompt) => setAssistant({ open: true, prompt }),
  };
  const ThemeIcon = theme === "light" ? Sun : theme === "dark" ? Moon : Monitor;
  const [page, a, b, c] = route.path;
  let body: ReactNode;
  let flush = false;
  switch (page ?? "") {
    case "":
    case "overview":
      body = <Overview />;
      break;
    case "inbox":
      body = <Inbox tab={a} />;
      break;
    case "deployments":
      body = a && b ? <DeploymentDetail namespace={a} name={b} tab={c} /> : <Deployments />;
      break;
    case "runs":
      body = <Runs query={route.query} />;
      break;
    case "trace":
      body = <TraceView traceId={a} span={route.query.get("span") ?? undefined} />;
      flush = true;
      break;
    case "costs":
      body = <Costs />;
      break;
    case "templates":
      body = <Templates selected={a} />;
      break;
    case "fleet":
      body = <Fleet />;
      break;
    case "settings":
      body = <Settings tab={a} />;
      break;
    case "new":
      body = <BundleEditor />;
      break;
    case "edit":
      body = <BundleEditor namespace={a} name={b} />;
      break;
    default:
      body = <Alert tone="info">No page here. Use the menu or press Ctrl+K.</Alert>;
  }

  return (
    <SessionContext.Provider value={session}>
      <div className="shell" data-menu={menu ? "open" : undefined}>
        <nav className="sidebar" aria-label="Main">
          <a className="brand" href="#/">
            <span className="brand-mark" aria-hidden>
              <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round">
                <path d="M3 13 8 3l5 10M5.2 9h5.6" />
              </svg>
            </span>
            agen
          </a>
          <ul className="nav">
            {NAV.map((n) => (
              <li key={n.id}>
                <a href={`#/${n.id}`} aria-current={n.match(route) ? "page" : undefined}>
                  <n.icon size={16} aria-hidden />
                  {n.label}
                  {n.id === "inbox" && inbox > 0 && (
                    <span className="count" aria-label={`${inbox} items`}>
                      {inbox}
                    </span>
                  )}
                </a>
              </li>
            ))}
          </ul>
          <ul className="nav sidebar-foot">
            <li>
              <a href="https://prjvvl.github.io/agen/" target="_blank" rel="noreferrer">
                <BookOpen size={16} aria-hidden />
                Docs
              </a>
            </li>
            <li>
              <a
                href="#"
                onClick={(e) => {
                  e.preventDefault();
                  nextTheme();
                }}
                aria-label={`Theme: ${theme}`}
              >
                <ThemeIcon size={16} aria-hidden />
                Theme: {theme}
              </a>
            </li>
            <li>
              <a
                href="#"
                onClick={(e) => {
                  e.preventDefault();
                  setToken("");
                  onSignOut();
                }}
              >
                <LogOut size={16} aria-hidden />
                Sign out{me?.name ? ` (${me.name})` : ""}
              </a>
            </li>
          </ul>
        </nav>
        <div className="main">
          <header className="topbar">
            <button className="btn ghost icon menu-btn" aria-label="Menu" onClick={() => setMenu((m) => !m)}>
              <Menu size={18} />
            </button>
            <button className="search-btn" onClick={() => setPalette(true)}>
              <Search size={15} aria-hidden />
              <span>Search or jump to…</span>
              <kbd>Ctrl K</kbd>
            </button>
            <div className="grow" />
            <span className="live" data-state={live} title={live === "live" ? "Live updates on" : live === "offline" ? "Live updates paused; retrying" : "Connecting"}>
              <span>{live === "live" ? "Live" : live === "offline" ? "Reconnecting" : "Connecting"}</span>
            </span>
            <button className="btn" onClick={() => setAssistant((s) => ({ open: !s.open }))} aria-expanded={assistant.open}>
              <MessageSquare size={15} aria-hidden />
              Assistant
            </button>
          </header>
          <main className={`content${flush ? " flush" : ""}`} id="main">
            <Boundary key={route.path.join("/")}>{body}</Boundary>
          </main>
        </div>
        {palette && <CommandPalette onClose={() => setPalette(false)} onTheme={nextTheme} />}
        {assistant.open && <Assistant prompt={assistant.prompt} onClose={() => setAssistant({ open: false })} />}
      </div>
    </SessionContext.Provider>
  );
}

/** Pending approvals plus tasks that failed since the inbox was last opened. */
function useInboxCount(approver: boolean): number {
  const [seen, setSeen] = useState(inboxSeen);
  useEffect(() => {
    const on = () => setSeen(inboxSeen());
    addEventListener("agen:inbox-seen", on);
    return () => removeEventListener("agen:inbox-seen", on);
  }, []);
  const approvals = useQuery(
    () => (approver ? call<{ approvals?: Approval[] }>("ListApprovals", { state: "APPROVAL_STATE_PENDING" }) : Promise.resolve({ approvals: [] })),
    [approver],
    { on: ["approval", "task"] },
  );
  const failed = useQuery(() => call<{ tasks?: Task[] }>("ListTasks", { state: "TASK_STATE_FAILED", limit: 50 }), [], { on: ["task"] });
  const recent = (failed.data?.tasks ?? []).filter((t) => new Date(t.updatedAt ?? t.createdAt ?? 0).getTime() > seen);
  return (approvals.data?.approvals?.length ?? 0) + recent.length;
}

/** When the inbox was last opened (default: a day ago). */
export function inboxSeen(): number {
  try {
    const v = Number(localStorage.getItem("agen.inboxSeen"));
    if (v) return v;
  } catch {
    /* storage unavailable */
  }
  return Date.now() - 86400_000;
}

export function markInboxSeen() {
  try {
    localStorage.setItem("agen.inboxSeen", String(Date.now()));
  } catch {
    /* storage unavailable */
  }
  dispatchEvent(new Event("agen:inbox-seen"));
}

// A page that throws shows the error instead of a blank screen.
class Boundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {};
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  render() {
    return this.state.error ? <Alert>Something went wrong: {this.state.error.message}</Alert> : this.props.children;
  }
}

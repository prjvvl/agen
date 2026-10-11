import { ArrowRight, Boxes, Hash, Plus, SunMoon } from "lucide-react";
import { Fragment, ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { call, Deployment, Task } from "../api";
import { useQuery } from "../live";
import { deploymentHref, go, traceHref } from "../router";

interface Item {
  id: string;
  group: string;
  label: string;
  icon: ReactNode;
  hint?: string;
  run: () => void | Promise<void>;
}

const PAGES: [string, string][] = [
  ["", "Overview"],
  ["inbox", "Inbox"],
  ["deployments", "Deployments"],
  ["runs", "Runs"],
  ["costs", "Costs"],
  ["templates", "Templates"],
  ["fleet", "Fleet"],
  ["settings", "Settings"],
];

export function CommandPalette({ onClose, onTheme }: { onClose: () => void; onTheme: () => void }) {
  const [q, setQ] = useState("");
  const [sel, setSel] = useState(0);
  const [error, setError] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const deps = useQuery(() => call<{ deployments?: Deployment[] }>("ListDeployments", {}), []);
  useEffect(() => input.current?.focus(), []);

  const items = useMemo(() => {
    const term = q.trim().toLowerCase();
    const out: Item[] = [];
    const id = q.trim();
    if (/^[0-9a-f]{32}$/i.test(id))
      out.push({ id: "trace", group: "Open", label: `Trace ${id}`, icon: <Hash size={15} />, run: () => void (location.hash = traceHref(id.toLowerCase())) });
    if (/^[0-9A-HJKMNP-TV-Z]{26}$/i.test(id))
      out.push({
        id: "task",
        group: "Open",
        label: `Task ${id}`,
        icon: <Hash size={15} />,
        run: async () => {
          const t = await call<{ task: Task }>("GetTask", { id: id.toUpperCase() });
          if (t.task.traceId) location.hash = traceHref(t.task.traceId);
          else go(["deployments", t.task.namespace, t.task.deployment, "tasks"]);
        },
      });
    const match = (s: string) => !term || s.toLowerCase().includes(term);
    for (const [path, label] of PAGES)
      if (match(label)) out.push({ id: "page-" + path, group: "Go to", label, icon: <ArrowRight size={15} />, run: () => void (location.hash = `#/${path}`) });
    for (const d of deps.data?.deployments ?? [])
      if (match(`${d.namespace}/${d.name}`))
        out.push({
          id: `dep-${d.namespace}/${d.name}`,
          group: "Deployments",
          label: d.name,
          hint: d.namespace,
          icon: <Boxes size={15} />,
          run: () => void (location.hash = deploymentHref(d.namespace, d.name)),
        });
    const actions: Item[] = [
      { id: "new", group: "Actions", label: "New agent", icon: <Plus size={15} />, run: () => void (location.hash = "#/new") },
      { id: "tpl", group: "Actions", label: "Deploy from a template", icon: <Plus size={15} />, run: () => void (location.hash = "#/templates") },
      { id: "theme", group: "Actions", label: "Switch theme", icon: <SunMoon size={15} />, run: onTheme },
      { id: "failed", group: "Actions", label: "Show failed runs", icon: <ArrowRight size={15} />, run: () => void (location.hash = "#/runs?status=failed&all=1") },
    ];
    out.push(...actions.filter((a) => match(a.label)));
    return out.slice(0, 50);
  }, [q, deps.data, onTheme]);

  useEffect(() => setSel(0), [q]);

  async function pick(it?: Item) {
    if (!it) return;
    try {
      await it.run();
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  let lastGroup = "";
  return (
    <>
      <div className="panel-backdrop" onClick={onClose} />
      <div className="palette" role="dialog" aria-modal="true" aria-label="Search and commands">
        <input
          ref={input}
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Jump to a page, a deployment, or paste a trace or task id"
          aria-label="Search"
          role="combobox"
          aria-expanded="true"
          aria-controls="palette-list"
          aria-activedescendant={items[sel] ? `pal-${items[sel].id}` : undefined}
          onKeyDown={(e) => {
            if (e.key === "Escape") onClose();
            else if (e.key === "ArrowDown") setSel((s) => Math.min(items.length - 1, s + 1));
            else if (e.key === "ArrowUp") setSel((s) => Math.max(0, s - 1));
            else if (e.key === "Enter") pick(items[sel]);
            else return;
            e.preventDefault();
          }}
        />
        <ul id="palette-list" role="listbox">
          {items.map((it, i) => {
            const head = it.group !== lastGroup;
            lastGroup = it.group;
            return (
              <Fragment key={it.id}>
                {head && (
                  <li className="group" role="presentation">
                    {it.group}
                  </li>
                )}
                <li id={`pal-${it.id}`} role="option" aria-selected={i === sel} onMouseMove={() => setSel(i)} onClick={() => pick(it)}>
                  {it.icon}
                  {it.label}
                  {it.hint && <span className="hint">{it.hint}</span>}
                </li>
              </Fragment>
            );
          })}
          {!items.length && (
            <li className="group" role="presentation">
              Nothing matches.
            </li>
          )}
        </ul>
        {error && (
          <p className="alert" role="alert" style={{ margin: 8 }}>
            {error}
          </p>
        )}
      </div>
    </>
  );
}

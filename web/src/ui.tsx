import {
  AlertTriangle,
  Ban,
  Check,
  CheckCircle2,
  CircleDashed,
  CircleDot,
  CirclePause,
  Clock,
  Copy as CopyIcon,
  Info,
  Loader2,
  Moon,
  X,
  XCircle,
} from "lucide-react";
import { ReactNode, useId, useLayoutEffect, useRef, useState } from "react";
import { Deployment, MetricsBucket } from "./api";
import { ago, clock } from "./format";

export type Tone = "ok" | "warn" | "err" | "info" | "muted";

interface StatusSpec {
  tone: Tone;
  label: string;
  icon: typeof Check;
  active?: boolean;
}

/** Status shown as icon + word (never colour alone). */
export function Status({ spec, children }: { spec: StatusSpec; children?: ReactNode }) {
  const Icon = spec.icon;
  return (
    <span className="status" data-tone={spec.tone} data-active={spec.active || undefined}>
      <Icon size={14} aria-hidden />
      <span>{children ?? spec.label}</span>
    </span>
  );
}

export function runStatus(status?: string): StatusSpec {
  switch (status) {
    case "succeeded":
      return { tone: "ok", label: "succeeded", icon: CheckCircle2 };
    case "failed":
      return { tone: "err", label: "failed", icon: XCircle };
    case "cancelled":
      return { tone: "muted", label: "cancelled", icon: Ban };
    case "waiting_approval":
      return { tone: "warn", label: "waiting for approval", icon: CirclePause };
    case "running":
      return { tone: "info", label: "running", icon: Loader2, active: true };
    case "unfinished":
      return { tone: "warn", label: "interrupted", icon: AlertTriangle };
    default:
      return { tone: "muted", label: status || "unknown", icon: CircleDashed };
  }
}

export function taskStatus(state: string, pendingApproval?: string): StatusSpec {
  const s = state.replace("TASK_STATE_", "").toLowerCase();
  if (pendingApproval) return runStatus("waiting_approval");
  switch (s) {
    case "queued":
      return { tone: "muted", label: "queued", icon: Clock };
    case "leased":
      return { tone: "info", label: "starting", icon: Loader2, active: true };
    default:
      return runStatus(s);
  }
}

export function deploymentStatus(d: Deployment): StatusSpec {
  if (d.paused) return { tone: "muted", label: "stopped", icon: CirclePause };
  if (d.budgetExhausted) return { tone: "warn", label: "daily budget used", icon: AlertTriangle };
  if ((d.ready ?? 0) > 0) return { tone: "ok", label: `running · ${d.ready}`, icon: CircleDot };
  if ((d.desired ?? 0) > 0) return { tone: "info", label: "starting", icon: Loader2, active: true };
  return { tone: "muted", label: "asleep", icon: Moon };
}

export function instanceStatus(state: string): StatusSpec {
  const s = state.replace("INSTANCE_STATE_", "").toLowerCase();
  switch (s) {
    case "ready":
      return { tone: "ok", label: "ready", icon: CircleDot };
    case "busy":
      return { tone: "info", label: "busy", icon: Loader2, active: true };
    case "starting":
      return { tone: "info", label: "starting", icon: Loader2, active: true };
    case "failed":
      return { tone: "err", label: "failed", icon: XCircle };
    default:
      return { tone: "muted", label: s, icon: CircleDashed };
  }
}

export function Badge({ tone, children, title }: { tone?: Tone; children: ReactNode; title?: string }) {
  return (
    <span className="badge" data-tone={tone} title={title}>
      {children}
    </span>
  );
}

/** A value with a copy button; long values truncate with the full text in a tooltip. */
export function Copy({ text, display, mono = true }: { text: string; display?: ReactNode; mono?: boolean }) {
  const [done, setDone] = useState(false);
  return (
    <span className="copy">
      <span className={`truncate${mono ? " mono" : ""}`} title={text}>
        {display ?? text}
      </span>
      <button
        type="button"
        aria-label={`Copy ${text}`}
        onClick={(e) => {
          e.stopPropagation();
          navigator.clipboard?.writeText(text).then(() => {
            setDone(true);
            setTimeout(() => setDone(false), 1200);
          });
        }}
      >
        {done ? <Check size={12} /> : <CopyIcon size={12} />}
      </button>
    </span>
  );
}

export function Time({ t }: { t?: string }) {
  if (!t) return <span className="muted">–</span>;
  return (
    <time dateTime={t} title={new Date(t).toLocaleString()} className="nowrap">
      {ago(t)}
    </time>
  );
}

export function At({ t }: { t?: string }) {
  return (
    <time dateTime={t} title={t ? new Date(t).toLocaleString() : undefined} className="nowrap">
      {clock(t)}
    </time>
  );
}

export function Empty({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty">
      <strong>{title}</strong>
      {children && <div>{children}</div>}
      {action}
    </div>
  );
}

export function Alert({ tone = "err", children }: { tone?: Tone; children: ReactNode }) {
  const Icon = tone === "ok" ? CheckCircle2 : tone === "info" ? Info : AlertTriangle;
  return (
    <div className="alert" data-tone={tone} role={tone === "err" ? "alert" : "status"}>
      <Icon size={16} aria-hidden />
      <div>{children}</div>
    </div>
  );
}

export function SkeletonRows({ rows = 4, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <div className="table-wrap" aria-busy="true" aria-label="Loading">
      <table>
        <tbody>
          {Array.from({ length: rows }, (_, i) => (
            <tr key={i}>
              {Array.from({ length: cols }, (_, j) => (
                <td key={j}>
                  <div className="skeleton" style={{ height: 12, width: j === 0 ? "60%" : "40%" }} />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** A side panel: the list stays in view; Esc or the close button dismisses it. */
export function Panel({
  title,
  onClose,
  children,
  footer,
  wide,
  head,
}: {
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  head?: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const id = useId();
  // Before paint: a panel that is visible already closes on Escape.
  useLayoutEffect(() => {
    const before = document.activeElement as HTMLElement | null;
    if (!ref.current?.contains(document.activeElement)) ref.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    addEventListener("keydown", onKey);
    return () => {
      removeEventListener("keydown", onKey);
      before?.focus?.();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <>
      <div className="panel-backdrop" onClick={onClose} />
      <div className={`panel${wide ? " wide" : ""}`} role="dialog" aria-modal="true" aria-labelledby={id} tabIndex={-1} ref={ref}>
        <div className="panel-head">
          <h2 id={id} className="truncate">
            {title}
          </h2>
          {head}
          <button className="btn ghost icon" onClick={onClose} aria-label="Close">
            <X size={16} />
          </button>
        </div>
        <div className="panel-body">{children}</div>
        {footer && <div className="panel-foot">{footer}</div>}
      </div>
    </>
  );
}

/** JSON with light syntax colouring. */
export function Json({ value, label }: { value: unknown; label?: string }) {
  let text: string;
  if (typeof value === "string") {
    try {
      text = JSON.stringify(JSON.parse(value), null, 2);
    } catch {
      return (
        <pre className="code" aria-label={label}>
          {value}
        </pre>
      );
    }
  } else {
    text = JSON.stringify(value ?? null, null, 2);
  }
  const parts: ReactNode[] = [];
  const re = /("(?:[^"\\]|\\.)*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|([{}[\],])/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let i = 0;
  while ((m = re.exec(text))) {
    if (m.index > last) parts.push(text.slice(last, m.index));
    if (m[1]) {
      parts.push(
        <span key={i++} className={m[2] ? "k" : "s"}>
          {m[1]}
        </span>,
      );
      if (m[2]) parts.push(m[2]);
    } else if (m[3] || m[4]) {
      parts.push(
        <span key={i++} className="n">
          {m[0]}
        </span>,
      );
    } else {
      parts.push(
        <span key={i++} className="p">
          {m[0]}
        </span>,
      );
    }
    last = re.lastIndex;
  }
  parts.push(text.slice(last));
  return (
    <pre className="code" aria-label={label}>
      {parts}
    </pre>
  );
}

/** A labelled bar of value against a limit. */
export function Meter({
  label,
  value,
  max,
  format = String,
  neutral,
}: {
  label: ReactNode;
  value: number;
  max?: number;
  format?: (n: number) => string;
  /** Being at the maximum is normal (no warning colour). */
  neutral?: boolean;
}) {
  const limited = !!max && max > 0;
  const ratio = limited ? Math.min(1, value / max!) : 0;
  const tone = neutral ? undefined : ratio >= 1 ? "err" : ratio >= 0.8 ? "warn" : undefined;
  return (
    <div className="meter" data-tone={tone}>
      <div className="row">
        <span className="soft">{label}</span>
        <span className="num">
          {format(value)}
          <span className="muted"> / {limited ? format(max!) : "no limit"}</span>
        </span>
      </div>
      <div
        className="track"
        role="meter"
        aria-valuenow={value}
        aria-valuemin={0}
        aria-valuemax={limited ? max : undefined}
        aria-label={typeof label === "string" ? label : undefined}
      >
        <div className="fill" style={{ width: `${ratio * 100}%` }} />
      </div>
    </div>
  );
}

/** Runs per time bucket, failures in red: a real count, not decoration. */
export function MiniBars({ buckets, label }: { buckets?: MetricsBucket[]; label: string }) {
  const list = buckets ?? [];
  const top = Math.max(1, ...list.map((b) => b.runs ?? 0));
  const total = list.reduce((a, b) => a + (b.runs ?? 0), 0);
  return (
    <div className="minibars" role="img" aria-label={`${label}: ${total} runs`}>
      {list.map((b, i) => (
        <span
          key={i}
          data-failed={(b.failed ?? 0) > 0 || undefined}
          style={{ height: `${((b.runs ?? 0) / top) * 100}%` }}
          title={`${b.runs ?? 0} runs${b.failed ? `, ${b.failed} failed` : ""}`}
        />
      ))}
    </div>
  );
}

export function Tabs<T extends string>({ tabs, value, onChange }: { tabs: [T, ReactNode][]; value: T; onChange: (v: T) => void }) {
  return (
    <div className="tabs" role="tablist">
      {tabs.map(([id, label]) => (
        <button key={id} role="tab" aria-selected={id === value} onClick={() => onChange(id)}>
          {label}
        </button>
      ))}
    </div>
  );
}

export function Segmented<T extends string>({ options, value, onChange, label }: { options: [T, string][]; value: T; onChange: (v: T) => void; label: string }) {
  return (
    <div className="segmented" role="group" aria-label={label}>
      {options.map(([id, text]) => (
        <button key={id} aria-pressed={id === value} onClick={() => onChange(id)}>
          {text}
        </button>
      ))}
    </div>
  );
}

/** A button that asks for confirmation in place (no modal). */
export function ConfirmButton({
  label,
  confirm,
  onConfirm,
  disabled,
  className = "btn",
}: {
  label: ReactNode;
  confirm: string;
  onConfirm: () => void;
  disabled?: boolean;
  className?: string;
}) {
  const [asking, setAsking] = useState(false);
  if (!asking)
    return (
      <button className={className} disabled={disabled} onClick={() => setAsking(true)}>
        {label}
      </button>
    );
  return (
    <span className="actions">
      <button
        className="btn danger solid"
        onClick={() => {
          setAsking(false);
          onConfirm();
        }}
      >
        {confirm}
      </button>
      <button className="btn ghost" onClick={() => setAsking(false)}>
        Keep
      </button>
    </span>
  );
}

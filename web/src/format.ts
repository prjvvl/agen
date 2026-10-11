import { Int, num, Usage } from "./api";

export const ms = (t?: string) => (t ? new Date(t).getTime() : 0);

export function duration(msValue: number): string {
  if (!Number.isFinite(msValue) || msValue < 0) return "–";
  if (msValue < 1000) return `${Math.round(msValue)} ms`;
  const s = msValue / 1000;
  if (s < 60) return `${s < 10 ? s.toFixed(1) : Math.round(s)} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function runDuration(start?: string, end?: string): string {
  if (!start) return "–";
  return duration((end ? ms(end) : Date.now()) - ms(start));
}

export function ago(t?: string, now = Date.now()): string {
  if (!t) return "–";
  const diff = (now - ms(t)) / 1000;
  const d = Math.abs(diff);
  const span = d < 3600 ? `${Math.max(1, Math.round(d / 60))}m` : d < 86400 ? `${Math.round(d / 3600)}h` : `${Math.round(d / 86400)}d`;
  if (d < 45) return diff >= 0 ? "just now" : "in a moment";
  return diff >= 0 ? `${span} ago` : `in ${span}`;
}

export function clock(t?: string): string {
  if (!t) return "";
  const d = new Date(t);
  const today = new Date().toDateString() === d.toDateString();
  return today
    ? d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" })
    : d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

export function money(v?: number): string {
  const n = v ?? 0;
  if (n === 0) return "$0";
  if (n < 0.01) return `$${n.toFixed(4)}`;
  if (n < 10) return `$${n.toFixed(2)}`;
  return `$${n.toFixed(0)}`;
}

export function compact(v?: Int): string {
  const n = num(v);
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

export const tokens = (u?: Usage) => num(u?.inputTokens) + num(u?.outputTokens);

export function pct(part: number, whole: number): string {
  if (!whole) return "–";
  const p = (100 * part) / whole;
  return p > 0 && p < 1 ? "<1%" : `${Math.round(p)}%`;
}

export const ns = (r: { namespace?: string; name: string }) => `${r.namespace ?? "default"}/${r.name}`;

export function enumName(v: string | undefined, prefix: string): string {
  return (v ?? "").replace(prefix, "").toLowerCase();
}

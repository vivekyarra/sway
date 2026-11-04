export function usd(cents: number | null | undefined, opts: { sign?: boolean } = {}): string {
  const c = cents ?? 0;
  const s = (Math.abs(c) / 100).toLocaleString("en-US", { style: "currency", currency: "USD" });
  if (c < 0) return `−${s}`;
  return opts.sign && c > 0 ? `+${s}` : s;
}

export function pct(x: number | null | undefined, digits = 0): string {
  if (x === null || x === undefined || Number.isNaN(x)) return "—";
  return `${(x * 100).toFixed(digits)}%`;
}

export function pts(x: number, digits = 1): string {
  const v = x * 100;
  return `${v >= 0 ? "+" : "−"}${Math.abs(v).toFixed(digits)} pts`;
}

export function ago(iso: string | null | undefined): string {
  if (!iso) return "—";
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 0) return `in ${dur(-s)}`;
  if (s < 45) return "just now";
  return `${dur(s)} ago`;
}

export function dur(seconds: number): string {
  if (seconds < 60) return `${Math.max(1, Math.round(seconds))}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)}h`;
  return `${Math.round(seconds / 86400)}d`;
}

export function shortDate(iso: string | null | undefined): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

export function initials(name: string): string {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]?.toUpperCase())
    .join("");
}

export const aspectLabel: Record<string, string> = {
  comfort: "comfort",
  anc: "noise cancelling",
  battery: "battery",
  portability: "portability",
  weight: "weight",
  durability: "durability",
  capacity: "capacity",
  stability: "stability",
  noise: "motor noise",
  height: "height range",
};

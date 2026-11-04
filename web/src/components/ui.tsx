"use client";

import type { ReactNode } from "react";
import { initials } from "@/lib/format";

const statusStyle: Record<string, string> = {
  Pending: "bg-pending-soft text-pending",
  Vested: "bg-vested-soft text-vested",
  Paid: "bg-paid-soft text-paid",
  SUCCESS: "bg-paid-soft text-paid",
  Forfeited: "bg-forfeit-soft text-forfeit",
  "Clawed back": "bg-forfeit-soft text-forfeit",
  Frozen: "bg-frozen-soft text-frozen",
  "No earnings": "bg-paper-2 text-muted",
  Settled: "bg-paper-2 text-muted",
  CAPTURED: "bg-paid-soft text-paid",
  REFUNDED: "bg-forfeit-soft text-forfeit",
  PARTIALLY_REFUNDED: "bg-pending-soft text-pending",
  CREATED: "bg-paper-2 text-muted",
  APPROVED: "bg-vested-soft text-vested",
  DONE: "bg-paid-soft text-paid",
  RUNNING: "bg-accent-soft text-accent-ink",
  QUEUED: "bg-paper-2 text-muted",
  FAILED: "bg-forfeit-soft text-forfeit",
  PENDING: "bg-pending-soft text-pending",
  UNCLAIMED: "bg-pending-soft text-pending",
  RETURNED: "bg-forfeit-soft text-forfeit",
  OPEN: "bg-frozen-soft text-frozen",
  active: "bg-paid-soft text-paid",
  blocked_injection: "bg-forfeit-soft text-forfeit",
  duplicate: "bg-forfeit-soft text-forfeit",
};

const statusLabel: Record<string, string> = {
  PARTIALLY_REFUNDED: "Partly refunded",
  blocked_injection: "Blocked",
  duplicate: "Duplicate",
  active: "Active",
  OPEN: "Dispute open",
};

export function StatusPill({ status, className = "" }: { status: string; className?: string }) {
  const style = statusStyle[status] ?? "bg-paper-2 text-ink-2";
  const label = statusLabel[status] ?? (status.length > 3 && status === status.toUpperCase() ? status.charAt(0) + status.slice(1).toLowerCase() : status);
  return <span className={`chip ${style} ${className}`}>{label}</span>;
}

export function Badge({ children, tone = "neutral", title }: { children: ReactNode; tone?: "neutral" | "accent" | "warn" | "sim" | "good"; title?: string }) {
  const tones = {
    neutral: "bg-paper-2 text-ink-2 border border-line",
    accent: "bg-accent-soft text-accent-ink",
    warn: "bg-pending-soft text-pending",
    sim: "bg-[#fff1f6] text-[#b4235c] border border-[#f7c9da]",
    good: "bg-paid-soft text-paid",
  };
  return (
    <span className={`chip ${tones[tone]}`} title={title}>
      {children}
    </span>
  );
}

export function Avatar({ name, hue = 250, size = 36 }: { name: string; hue?: number; size?: number }) {
  return (
    <span
      aria-hidden
      className="inline-flex shrink-0 items-center justify-center rounded-full font-semibold text-white"
      style={{
        width: size,
        height: size,
        fontSize: size * 0.38,
        background: `linear-gradient(135deg, hsl(${hue} 70% 55%), hsl(${(hue + 40) % 360} 65% 42%))`,
      }}
    >
      {initials(name)}
    </span>
  );
}

const toneText = { pending: "text-pending", vested: "text-vested", paid: "text-paid", forfeit: "text-forfeit", frozen: "text-frozen", accent: "text-accent" };
const toneBar = { pending: "bg-pending", vested: "bg-vested", paid: "bg-paid", forfeit: "bg-forfeit", frozen: "bg-frozen", accent: "bg-accent" };

export function Stat({ label, value, tone, sub }: { label: string; value: ReactNode; tone?: "pending" | "vested" | "paid" | "forfeit" | "frozen" | "accent"; sub?: ReactNode }) {
  const color = tone ? toneText[tone] : "text-ink";
  const bar = tone ? toneBar[tone] : "bg-ink";
  return (
    <div className="card relative overflow-hidden p-4">
      <span className={`absolute inset-x-0 top-0 h-1 ${bar} opacity-80`} />
      <div className="eyebrow">{label}</div>
      <div className={`tabular mt-1.5 text-2xl font-semibold ${color}`}>{value}</div>
      {sub && <div className="mt-1 text-xs text-muted">{sub}</div>}
    </div>
  );
}

export function Spinner({ className = "" }: { className?: string }) {
  return (
    <svg className={`h-4 w-4 animate-spin ${className}`} viewBox="0 0 24 24" fill="none" aria-hidden>
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeOpacity="0.2" strokeWidth="3" />
      <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    </svg>
  );
}

export function ErrorBox({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div role="alert" className="flex items-start gap-3 rounded-xl border border-[#f3c3bf] bg-forfeit-soft px-4 py-3 text-sm text-forfeit">
      <span className="mt-0.5">⚠</span>
      <span className="flex-1">{message}</span>
      {onRetry && (
        <button className="font-semibold underline" onClick={onRetry}>
          Retry
        </button>
      )}
    </div>
  );
}

export function SectionTitle({ eyebrow, title, right }: { eyebrow?: string; title: ReactNode; right?: ReactNode }) {
  return (
    <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
      <div>
        {eyebrow && <div className="eyebrow mb-1">{eyebrow}</div>}
        <h2 className="text-lg font-semibold tracking-tight">{title}</h2>
      </div>
      {right}
    </div>
  );
}

export function SourceIcon({ type }: { type: string }) {
  const map: Record<string, { label: string; path: ReactNode }> = {
    blog: { label: "Blog", path: <path d="M4 5h16M4 10h16M4 15h10M4 20h7" /> },
    review: { label: "Review", path: <path d="M12 3l2.7 5.6 6.1.9-4.4 4.3 1 6.1L12 17l-5.4 2.9 1-6.1L3.2 9.5l6.1-.9z" /> },
    forum: { label: "Forum", path: <path d="M4 5h16v10H9l-5 4z" /> },
    transcript: { label: "Video", path: <path d="M4 6h12v12H4zM16 10l4-2v8l-4-2" /> },
  };
  const m = map[type] ?? map.blog;
  return (
    <span title={m.label} className="inline-flex h-6 w-6 items-center justify-center rounded-md bg-paper-2 text-ink-2">
      <svg viewBox="0 0 24 24" className="h-3.5 w-3.5" fill="none" stroke="currentColor" strokeWidth="2" strokeLinejoin="round" strokeLinecap="round">
        {m.path}
      </svg>
    </span>
  );
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="rounded-2xl border border-dashed border-line-2 px-6 py-10 text-center">
      <div className="font-semibold">{title}</div>
      {children && <div className="mt-1 text-sm text-muted">{children}</div>}
    </div>
  );
}

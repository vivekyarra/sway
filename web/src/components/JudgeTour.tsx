"use client";

import Link from "next/link";
import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { resetTour, TOUR_STEPS, tourStore } from "@/lib/tour";
import { useConfig } from "./Providers";

export function JudgeTour() {
  const cfg = useConfig();
  const [open, setOpen] = useState(false);
  const raw = useSyncExternalStore(tourStore.subscribe, tourStore.snapshot, tourStore.server);
  const done = useMemo<number[]>(() => {
    try {
      return JSON.parse(raw);
    } catch {
      return [];
    }
  }, [raw]);

  useEffect(() => {
    const onOpen = () => setOpen(true);
    window.addEventListener("sway:tour-open", onOpen);
    return () => window.removeEventListener("sway:tour-open", onOpen);
  }, []);

  const count = done.length;
  return (
    <div className="fixed bottom-4 right-4 z-50 w-[min(360px,calc(100vw-2rem))]">
      {open ? (
        <section aria-label="Judge tour" className="card rise overflow-hidden shadow-[var(--shadow-lg)]">
          <div className="flex items-center justify-between border-b border-line bg-ink px-4 py-3 text-white">
            <div>
              <div className="text-sm font-semibold">3-minute judge tour</div>
              <div className="text-xs text-white/60">{count} of {TOUR_STEPS.length} done · steps tick automatically</div>
            </div>
            <button aria-label="Close tour" className="rounded-full px-2 py-1 text-white/70 hover:bg-white/10" onClick={() => setOpen(false)}>
              ✕
            </button>
          </div>
          <div className="h-1 bg-paper-2">
            <div className="h-1 bg-accent transition-all" style={{ width: `${(count / TOUR_STEPS.length) * 100}%` }} />
          </div>
          <ol className="max-h-[52vh] overflow-y-auto p-2">
            {TOUR_STEPS.map((s) => {
              const ok = done.includes(s.id);
              return (
                <li key={s.id}>
                  <Link href={s.href} className="flex gap-3 rounded-xl px-2 py-2 hover:bg-paper-2" onClick={() => window.innerWidth < 640 && setOpen(false)}>
                    <span
                      className={`mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full text-[11px] font-bold ${ok ? "bg-paid text-white" : "border border-line-2 text-muted"}`}
                    >
                      {ok ? "✓" : s.id}
                    </span>
                    <span>
                      <span className={`block text-sm font-medium ${ok ? "text-muted line-through decoration-line-2" : ""}`}>{s.title}</span>
                      <span className="block text-xs text-muted">{s.hint}</span>
                    </span>
                  </Link>
                  {s.id === 3 && cfg?.paypal.mode === "sandbox" && cfg.paypal.buyer_email && (
                    <div className="mx-2 mb-2 ml-10 rounded-lg bg-paper-2 px-3 py-2 font-mono text-[11px] leading-5 text-ink-2">
                      buyer: {cfg.paypal.buyer_email}
                      <br />
                      password: {cfg.paypal.buyer_password}
                    </div>
                  )}
                  {s.id === 3 && cfg?.paypal.mode === "sim" && (
                    <div className="mx-2 mb-2 ml-10 rounded-lg bg-paper-2 px-3 py-2 text-[11px] leading-5 text-ink-2">
                      Running without PayPal keys: checkout uses the labelled PayPal simulator.
                    </div>
                  )}
                </li>
              );
            })}
          </ol>
          <div className="flex justify-between border-t border-line px-4 py-2 text-xs">
            <button className="text-muted hover:text-ink" onClick={() => resetTour()}>
              Restart checklist
            </button>
            <Link href="/#how" className="link">
              How Sway works
            </Link>
          </div>
        </section>
      ) : (
        <button onClick={() => setOpen(true)} className="btn btn-primary ml-auto flex shadow-[var(--shadow-lg)]" aria-expanded={false}>
          <span className="flex h-5 w-5 items-center justify-center rounded-full bg-white/15 text-[11px]">{count}/8</span>
          Judge tour
        </button>
      )}
    </div>
  );
}

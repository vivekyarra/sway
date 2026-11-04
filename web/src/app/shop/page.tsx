"use client";

import { useRef, useState } from "react";
import { Checkout } from "@/components/Checkout";
import { ProductArt } from "@/components/ProductArt";
import { useConfig } from "@/components/Providers";
import { ErrorBox, SourceIcon, Spinner } from "@/components/ui";
import { WhyPanel } from "@/components/WhyPanel";
import { api, ApiError } from "@/lib/api";
import { pct, usd } from "@/lib/format";
import { completeStep } from "@/lib/tour";
import type { Decision } from "@/lib/types";

const STAGES = ["Finding products", "Reading what people wrote", "Deciding"];

function specLine(specs: Record<string, string | number | boolean>) {
  const nice: Record<string, (v: string | number | boolean) => string> = {
    battery_hours: (v) => `${v} h battery`,
    weight_g: (v) => `${v} g`,
    capacity_l: (v) => `${v} L`,
    weight_kg: (v) => `${v} kg`,
    motors: (v) => `${v} motor${v === 1 ? "" : "s"}`,
    height_range_cm: (v) => `${v} cm`,
  };
  return Object.entries(specs)
    .slice(0, 4)
    .map(([k, v]) => (nice[k] ? nice[k](v) : typeof v === "boolean" ? (v ? k.replace(/_/g, " ") : "") : String(v)))
    .filter(Boolean)
    .join(" · ");
}

export default function ShopPage() {
  const cfg = useConfig();
  const [typed, setText] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [stage, setStage] = useState(-1);
  const [decision, setDecision] = useState<Decision | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const prompts = cfg?.categories ?? [];

  const text = typed ?? prompts.find((p) => p.key === "headphones")?.suggested_prompt ?? "";

  async function ask(q: string) {
    if (!q.trim() || busy) return;
    setBusy(true);
    setError(null);
    setDecision(null);
    setStage(0);
    const timers = [setTimeout(() => setStage(1), 700), setTimeout(() => setStage(2), 1500)];
    try {
      const d = await api.post<Decision>("/agent/decide", { request: q });
      timers.forEach(clearTimeout);
      // Reveal the agent's work in the order it happened.
      setStage(1);
      setDecision(d);
      setTimeout(() => setStage(2), 350);
      setTimeout(() => setStage(3), 800);
      if (d.category === "headphones") completeStep(1);
    } catch (e) {
      timers.forEach(clearTimeout);
      setStage(-1);
      setError(e instanceof ApiError ? e : new ApiError(0, "error", String(e)));
    } finally {
      setBusy(false);
    }
  }

  const chosenProb = decision ? decision.probabilities[decision.chosen.id] : 0;

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6">
      <div className="mb-6">
        <div className="eyebrow">Shopper</div>
        <h1 className="display mt-1 text-4xl sm:text-5xl">Ask the shopping agent</h1>
        <p className="mt-2 max-w-2xl text-ink-2">A neutral agent reads what real people wrote, then picks. You see exactly which writing changed its mind.</p>
      </div>

      <form
        className="card p-3 sm:p-4"
        onSubmit={(e) => {
          e.preventDefault();
          ask(text);
        }}
      >
        <label htmlFor="ask" className="sr-only">
          What are you shopping for?
        </label>
        <div className="flex flex-col gap-3 sm:flex-row sm:items-end">
          <textarea
            id="ask"
            ref={inputRef}
            rows={2}
            maxLength={500}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                ask(text);
              }
            }}
            placeholder="I need noise-cancelling headphones for long-haul flights, under $250."
            className="min-h-[56px] flex-1 resize-none rounded-xl bg-paper px-4 py-3 text-[15px] outline-none ring-1 ring-line focus:ring-2 focus:ring-accent"
          />
          <button type="submit" className="btn btn-accent h-12 px-6" disabled={busy || !text.trim()}>
            {busy ? <Spinner /> : null} {busy ? "Thinking…" : "Ask agent"}
          </button>
        </div>
        <div className="mt-3 flex flex-wrap gap-2">
          {prompts.map((p) => (
            <button
              key={p.key}
              type="button"
              className="chip border border-line bg-card px-3 py-1 text-ink-2 hover:border-line-2 hover:bg-paper-2"
              onClick={() => {
                setText(p.suggested_prompt);
                ask(p.suggested_prompt);
              }}
            >
              {p.label}
            </button>
          ))}
          <button type="button" className="chip border border-line bg-card px-3 py-1 text-ink-2 hover:bg-paper-2" onClick={() => {
            const q = "The cheapest on-ear headphones you have.";
            setText(q);
            ask(q);
          }}>
            A case where content doesn&apos;t matter
          </button>
        </div>
      </form>

      {error && (
        <div className="mt-4">
          <ErrorBox message={error.message} onRetry={() => ask(text)} />
          {error.suggestions && (
            <div className="mt-2 text-sm text-muted">
              Try:{" "}
              {prompts.map((p) => (
                <button key={p.key} className="link mr-3" onClick={() => { setText(p.suggested_prompt); ask(p.suggested_prompt); }}>
                  {p.label.toLowerCase()}
                </button>
              ))}
            </div>
          )}
        </div>
      )}

      {stage >= 0 && (
        <ol className="mt-6 flex flex-wrap gap-2" aria-live="polite">
          {STAGES.map((s, i) => (
            <li key={s} className={`chip px-3 py-1.5 text-[13px] ${stage > i ? "bg-paid-soft text-paid" : stage === i ? "bg-accent-soft text-accent-ink" : "bg-paper-2 text-muted"}`}>
              {stage > i ? "✓" : stage === i ? <span className="pulse-dot">●</span> : "○"} {s}
            </li>
          ))}
        </ol>
      )}

      {busy && !decision && (
        <div className="mt-4 grid gap-3 sm:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <div key={i} className="skeleton h-32" />
          ))}
        </div>
      )}

      {decision && stage >= 1 && (
        <div className="mt-4 space-y-6">
          <section aria-label="Candidates">
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              {decision.candidates.map((p) => {
                const chosen = stage >= 3 && p.id === decision.chosen.id;
                const over = decision.budget_cents > 0 && p.price_cents > decision.budget_cents;
                return (
                  <div key={p.id} className={`card rise p-3 transition ${chosen ? "ring-2 ring-accent" : stage >= 3 ? "opacity-70" : ""}`}>
                    <ProductArt id={p.id} category={p.category} className="h-24 w-full" />
                    <div className="mt-2 flex items-start justify-between gap-2">
                      <div>
                        <div className="font-semibold">{p.name}</div>
                        <div className="text-xs text-muted">{p.tagline}</div>
                      </div>
                      <div className="tabular text-right font-semibold">{usd(p.price_cents)}</div>
                    </div>
                    <div className="mt-1 text-[11px] text-muted">{specLine(p.specs)}</div>
                    <div className="mt-2 flex items-center justify-between text-xs">
                      {over ? <span className="chip bg-pending-soft text-pending">over budget</span> : <span />}
                      {stage >= 3 && <span className="tabular text-muted">{pct(decision.probabilities[p.id])} chance picked</span>}
                    </div>
                  </div>
                );
              })}
            </div>
          </section>

          {stage >= 2 && (
            <section aria-label="Sources read" className="rise flex flex-wrap items-center gap-2">
              <span className="text-sm text-muted">Read {decision.sources.length} sources:</span>
              {decision.sources.map((s) => (
                <span key={s.id} className="chip border border-line bg-card py-1 pl-1 pr-2.5 text-ink-2">
                  <SourceIcon type={s.type} /> {s.creator_name}
                </span>
              ))}
              {(decision.ignored_sources ?? []).map((s) => (
                <span key={s.id} className="chip border border-[#f3c3bf] bg-forfeit-soft py-1 pl-1 pr-2.5 text-forfeit line-through" title={s.reason}>
                  <SourceIcon type={s.type} /> {s.creator_name}
                </span>
              ))}
            </section>
          )}

          {stage >= 3 && (
            <div className="grid items-start gap-6 lg:grid-cols-[0.9fr_1.1fr]">
              <section aria-label="The agent's pick" className="card rise overflow-hidden lg:sticky lg:top-20">
                <div className="bg-ink px-5 py-3 text-sm text-white/80">
                  The agent picked <span className="font-semibold text-white">{decision.chosen.name}</span> · {pct(chosenProb)} confident
                </div>
                <div className="p-5">
                  <div className="flex gap-4">
                    <ProductArt id={decision.chosen.id} category={decision.chosen.category} className="h-24 w-32 shrink-0" />
                    <div>
                      <div className="text-xl font-semibold">{decision.chosen.name}</div>
                      <div className="text-sm text-muted">{decision.chosen.tagline}</div>
                      <div className="tabular mt-1 text-2xl font-semibold">{usd(decision.chosen.price_cents)}</div>
                    </div>
                  </div>
                  <p className="mt-4 text-[15px] leading-relaxed text-ink-2">{decision.rationale}</p>
                  <div className="mt-5">
                    <Checkout decisionId={decision.id} amountCents={decision.chosen.price_cents} productName={decision.chosen.name} />
                  </div>
                  <p className="mt-3 text-xs text-muted">
                    {cfg ? `${(cfg.commission_rate_bps / 100).toFixed(0)}% merchant commission is split among the creators who swayed this choice, and vests only if you keep it.` : ""}
                  </p>
                  <div className="mt-4 border-t border-line pt-3 text-[11px] text-muted">
                    Model: {decision.engine === "claude" ? decision.model : "Sway offline engine"} · prompt {decision.prompt_version} · decision {decision.id}
                  </div>
                </div>
              </section>
              <WhyPanel key={decision.id} decision={decision} />
            </div>
          )}
        </div>
      )}
    </div>
  );
}

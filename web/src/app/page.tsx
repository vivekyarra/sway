"use client";

import Link from "next/link";
import { ConnectAgent } from "@/components/ConnectAgent";
import { usePoll } from "@/lib/hooks";
import { usd } from "@/lib/format";
import type { Merchant } from "@/lib/types";

const steps = [
  { n: "01", title: "An agent decides", body: "A neutral shopping agent reads reviews, blogs, forum answers and video transcripts, then picks a product. It earns nothing for its choice." },
  { n: "02", title: "Sway re-runs the decision", body: "After checkout, Sway replays the agent's decision over every combination of sources and computes each one's exact Shapley value: its causal share of the purchase." },
  { n: "03", title: "Commission is split by influence", body: "The merchant's commission is divided among the creators whose words actually changed the outcome. Copied content, agent-manipulation attempts and opposing views earn $0." },
  { n: "04", title: "It vests only if the buyer keeps it", body: "PayPal refund and dispute webhooks decide. A return forfeits the commission; a dispute freezes it; a refund after payout becomes a clawback against future earnings." },
  { n: "05", title: "PayPal Payouts pays the creator", body: "Vested earnings go straight to the creator's PayPal email. Every cent is recorded in a double-entry, hash-chained ledger that always balances." },
];

export default function Home() {
  const { data } = usePoll<Merchant>("/merchant/overview", 15000);
  const k = data?.kpis;
  return (
    <div className="overflow-hidden">
      <section className="relative mx-auto max-w-7xl px-4 pb-16 pt-14 sm:px-6 sm:pt-20">
        <div aria-hidden className="pointer-events-none absolute -right-40 -top-24 h-[480px] w-[480px] rounded-full bg-[radial-gradient(circle,rgba(90,63,242,0.16),transparent_65%)]" />
        <div className="eyebrow mb-4 flex items-center gap-2">
          <span className="h-1.5 w-1.5 rounded-full bg-accent" /> Agentic commerce · PayPal · AI agents
        </div>
        <h1 className="display max-w-4xl text-[2.7rem] sm:text-7xl">
          AI agents shop now. <span className="italic text-accent">The people who taught them</span> should get paid.
        </h1>
        <p className="mt-6 max-w-2xl text-lg leading-relaxed text-ink-2">
          When an AI shopping agent buys something because of what a human wrote, Sway measures exactly whose content changed the decision and pays them through PayPal,
          but only if the buyer keeps the product.
        </p>
        <div className="mt-8 flex flex-wrap items-center gap-3">
          <Link href="/shop/" className="btn btn-accent px-6 py-3 text-base">
            Try it as a shopper →
          </Link>
          <button className="btn btn-ghost px-5 py-3 text-base" onClick={() => window.dispatchEvent(new Event("sway:tour-open"))}>
            I&apos;m a judge: 3-minute tour
          </button>
        </div>

        <ul className="mt-12 grid max-w-3xl grid-cols-2 gap-px overflow-hidden rounded-2xl border border-line bg-line sm:grid-cols-4" aria-label="Live marketplace totals">
          {[
            ["Agent-driven sales", k ? usd(k.agent_revenue_cents) : "…"],
            ["Paid to creators", k ? usd(k.commission_paid_cents) : "…"],
            ["Forfeited on returns", k ? usd(k.commission_forfeited_cents) : "…"],
            ["Ledger", data ? (data.verification.ok ? "Verified ✓" : "Broken ✕") : "…"],
          ].map(([label, value]) => (
            <li key={label} className="bg-card px-4 py-3">
              <span className="block text-xs text-muted">{label}</span>
              <span className="tabular mt-0.5 block text-lg font-semibold">{value}</span>
            </li>
          ))}
        </ul>
      </section>

      <section className="border-y border-line bg-card">
        <div className="mx-auto grid max-w-7xl gap-4 px-4 py-12 sm:px-6 md:grid-cols-3">
          {[
            { who: "Shoppers", line: "See why your agent chose.", body: "A “Why I chose this” panel shows which sources moved the decision. Remove one and watch the pick change.", href: "/shop/", cta: "Ask the agent" },
            { who: "Creators", line: "Get paid when your words sway a sale.", body: "Earnings per purchase you actually influenced, with a plain-language explanation, paid to your PayPal.", href: "/creator/?id=c_priya", cta: "Priya's dashboard" },
            { who: "Merchants", line: "Pay for influence on sales that stick.", body: "Commission only for causal influence on kept purchases. See which content drives sales, and which drives returns.", href: "/merchant/", cta: "Merchant console" },
          ].map((c) => (
            <Link key={c.who} href={c.href} className="group rounded-2xl border border-line p-6 transition hover:border-line-2 hover:shadow-[var(--shadow)]">
              <div className="eyebrow">{c.who}</div>
              <div className="display mt-2 text-3xl">{c.line}</div>
              <p className="mt-3 text-sm leading-relaxed text-ink-2">{c.body}</p>
              <div className="mt-5 text-sm font-semibold text-accent-ink group-hover:underline">{c.cta} →</div>
            </Link>
          ))}
        </div>
      </section>

      <section id="how" className="mx-auto max-w-7xl scroll-mt-20 px-4 py-16 sm:px-6">
        <div className="grid gap-10 lg:grid-cols-[1fr_1.4fr]">
          <div>
            <div className="eyebrow">How it works</div>
            <h2 className="display mt-2 text-4xl sm:text-5xl">
              Pay for content that <span className="italic">changed</span> what was bought.
            </h2>
            <p className="mt-4 leading-relaxed text-ink-2">
              Affiliate links pay for clicks; agents don&apos;t click. Agent-side commissions pay the agent, which biases it. Pay-per-crawl pays for reading, whether or not it
              mattered. Sway pays only for measured causal influence on purchases that buyers keep.
            </p>
            <div className="mt-6 rounded-2xl border border-line bg-card p-5 text-sm">
              <div className="font-semibold">The math, in one line</div>
              <div className="mt-2 font-mono text-[13px] leading-6 text-ink-2">
                v(S) = P(agent picks the purchased product | sources S)
                <br />
                φᵢ = Σ<sub>S</sub> |S|!(n−|S|−1)!/n! · [v(S∪i) − v(S)]
                <br />
                share = (v(all) − v(none)) / v(all)
              </div>
            </div>
          </div>
          <ol className="grid gap-3">
            {steps.map((s) => (
              <li key={s.n} className="card flex gap-4 p-5">
                <span className="display text-3xl text-accent">{s.n}</span>
                <div>
                  <div className="font-semibold">{s.title}</div>
                  <p className="mt-1 text-sm leading-relaxed text-ink-2">{s.body}</p>
                </div>
              </li>
            ))}
          </ol>
        </div>
      </section>

      <ConnectAgent />

      <footer className="border-t border-line">
        <div className="mx-auto flex max-w-7xl flex-wrap items-center justify-between gap-3 px-4 py-6 text-xs text-muted sm:px-6">
          <span>Sway · Built for “Build What&apos;s Next with PayPal and AI”. Runs on PayPal Sandbox. Every brand and creator in the catalog is original to Sway.</span>
          <span>
            <a className="link" href="https://github.com/vivekyarra/sway">Source (MIT)</a>
          </span>
        </div>
      </footer>
    </div>
  );
}

"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Suspense, useEffect, useState } from "react";
import { CoalitionExplorer } from "@/components/CoalitionExplorer";
import { useConfig } from "@/components/Providers";
import { Avatar, Badge, Empty, ErrorBox, SourceIcon, Spinner, StatusPill } from "@/components/ui";
import { api } from "@/lib/api";
import { usePoll, useNow } from "@/lib/hooks";
import { dur, pct, usd } from "@/lib/format";
import { completeStep } from "@/lib/tour";
import type { Order } from "@/lib/types";

function SplitBar({ o }: { o: Order }) {
  const s = o.split;
  if (!s.commission_cents) return null;
  const parts = [
    { label: "Creators", v: s.creator_pool_net_cents, cls: "bg-accent" },
    { label: "Sway fee", v: s.sway_fee_cents, cls: "bg-ink-2" },
    { label: "Back to merchant", v: s.returned_to_merchant_cents, cls: "bg-line-2" },
  ];
  return (
    <div>
      <div className="flex h-3 overflow-hidden rounded-full bg-paper-2" role="img" aria-label="Commission split">
        {parts.map((p) => (
          <div key={p.label} className={p.cls} style={{ width: `${(p.v / s.commission_cents) * 100}%` }} />
        ))}
      </div>
      <div className="mt-2 grid grid-cols-3 gap-2 text-xs">
        {parts.map((p) => (
          <div key={p.label}>
            <div className="flex items-center gap-1.5 text-muted">
              <span className={`h-2 w-2 rounded-full ${p.cls}`} /> {p.label}
            </div>
            <div className="tabular font-semibold">{usd(p.v)}</div>
          </div>
        ))}
      </div>
      <div className={`mt-3 flex items-center gap-2 text-xs ${s.balanced ? "text-paid" : "text-forfeit"}`}>
        {s.balanced ? "✓" : "✕"} {usd(s.creator_pool_net_cents)} + {usd(s.sway_fee_cents)} + {usd(s.returned_to_merchant_cents)} = {usd(s.commission_cents)} commission
      </div>
    </div>
  );
}

function OrderView({ id }: { id: string }) {
  const cfg = useConfig();
  const now = useNow();
  const { data: o, error, refresh } = usePoll<Order>(`/orders/${id}`, (d) => (!d || !d.attribution || d.attribution.status === "QUEUED" || d.attribution.status === "RUNNING" ? 600 : 4000));
  const [refunding, setRefunding] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  useEffect(() => {
    if (o?.attribution?.status === "DONE" && !o.seeded) completeStep(4);
    if (o?.vested_at && !o.seeded) completeStep(5);
    if (o && o.refunded_cents > 0 && !o.seeded) completeStep(7);
  }, [o]);

  if (error) return <ErrorBox message={error.status === 404 ? "Order not found." : error.message} onRetry={refresh} />;
  if (!o) return <div className="skeleton h-96" />;

  const job = o.attribution;
  const running = job && (job.status === "QUEUED" || job.status === "RUNNING");
  const prog = job && job.total_runs > 0 ? Math.min(1, job.progress / job.total_runs) : 0;
  const vestsIn = o.vests_at ? (new Date(o.vests_at).getTime() - now) / 1000 : 0;
  const productNames: Record<string, string> = { [o.product_id]: o.product_name };
  for (const r of o.attributions ?? []) productNames[r.source_id] = r.creator_name;
  const byCreator = new Map<string, number>();
  for (const r of o.attributions ?? []) byCreator.set(r.creator_id, (byCreator.get(r.creator_id) ?? 0) + r.amount_cents);

  async function refund(amount?: number) {
    if (!confirm(amount ? `Refund ${usd(amount)} of this order?` : "Refund this order in full?")) return;
    setRefunding(true);
    setMsg(null);
    try {
      await api.post(`/merchant/orders/${o!.id}/refund`, amount ? { amount_cents: amount, reason: "requested from order page" } : { reason: "requested from order page" });
      setMsg("Refund issued. Pending commission for this order is forfeited.");
      if (!o!.seeded) completeStep(7);
      refresh();
    } catch (e) {
      setMsg((e as Error).message);
    } finally {
      setRefunding(false);
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <div className="eyebrow">Order</div>
          <h1 className="display mt-1 text-4xl">{o.product_name}</h1>
          <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
            <StatusPill status={o.status} />
            {o.dispute_status !== "NONE" && <StatusPill status={o.dispute_status === "OPEN" ? "OPEN" : o.dispute_status} />}
            {o.dispute_simulated && <Badge tone="sim">simulated dispute</Badge>}
            {o.seeded ? <Badge>seeded history</Badge> : o.payment_mode === "sim" ? <Badge tone="sim">PayPal simulator</Badge> : <Badge tone="good">PayPal sandbox</Badge>}
            <span className="tabular font-semibold">{usd(o.amount_cents)}</span>
            {o.refunded_cents > 0 && <span className="text-forfeit">· {usd(o.refunded_cents)} refunded</span>}
          </div>
        </div>
        {cfg?.demo_mode && (o.status === "CAPTURED" || o.status === "PARTIALLY_REFUNDED") && (
          <div className="flex gap-2">
            <button className="btn btn-ghost btn-sm" disabled={refunding} onClick={() => refund(Math.floor((o.amount_cents - o.refunded_cents) / 2))}>
              Refund 50%
            </button>
            <button className="btn btn-ghost btn-sm text-forfeit" disabled={refunding} onClick={() => refund()}>
              {refunding ? <Spinner /> : null} Request refund
            </button>
          </div>
        )}
      </div>
      {msg && <div className="rounded-xl bg-paper-2 px-4 py-3 text-sm">{msg}</div>}

      <div className="grid gap-6 lg:grid-cols-[1.25fr_0.75fr]">
        <div className="min-w-0 space-y-6">
          <section className="card p-5 sm:p-6" aria-labelledby="attr">
            <div className="flex flex-wrap items-baseline justify-between gap-2">
              <h2 id="attr" className="text-lg font-semibold tracking-tight">
                Who swayed this sale
              </h2>
              {job && <StatusPill status={job.status} />}
            </div>
            {!job && <p className="mt-2 text-sm text-muted">Attribution starts as soon as the payment is captured.</p>}
            {running && (
              <div className="mt-4" aria-live="polite">
                <div className="flex justify-between text-sm">
                  <span>
                    {job!.phase === "screening" ? "Screening sources (leave-one-out)…" : job!.phase === "coalitions" ? "Re-running the decision over every coalition…" : "Queued…"}
                  </span>
                  <span className="tabular text-muted">
                    Re-running decision {job!.progress} of {job!.total_runs || "…"}
                  </span>
                </div>
                <div className="mt-2 h-2 overflow-hidden rounded-full bg-paper-2">
                  <div className="h-2 rounded-full bg-accent transition-all duration-500" style={{ width: `${Math.max(4, prog * 100)}%` }} />
                </div>
                <div className="mt-1 text-xs text-muted">{job!.cache_hits} served from the run cache</div>
              </div>
            )}
            {job?.status === "FAILED" && <div className="mt-3"><ErrorBox message={`Attribution failed: ${job.error}. It will retry automatically.`} /></div>}
            {job?.status === "DONE" && (
              <>
                <div className="mt-3 grid grid-cols-1 gap-3 text-sm sm:grid-cols-3">
                  <div className="rounded-xl bg-paper-2 p-3">
                    <div className="text-xs text-muted">Influence share</div>
                    <div className="tabular text-xl font-semibold">{pct(job.share)}</div>
                  </div>
                  <div className="rounded-xl bg-paper-2 p-3">
                    <div className="text-xs text-muted">v(none) → v(all)</div>
                    <div className="tabular text-xl font-semibold">
                      {pct(job.v_none)} → {pct(job.v_full)}
                    </div>
                  </div>
                  <div className="rounded-xl bg-paper-2 p-3">
                    <div className="text-xs text-muted">Method</div>
                    <div className="text-sm font-semibold">{job.method === "exact" ? "Exact Shapley" : "Monte Carlo (approx.)"}</div>
                    <div className="tabular text-[11px] text-muted">{job.progress} runs · Σφ gap {(job.efficiency_gap ?? 0).toExponential(0)}</div>
                  </div>
                </div>
                {(job.share ?? 0) === 0 && (
                  <p className="mt-3 rounded-xl border border-line bg-paper px-4 py-3 text-sm">
                    The human-written sources didn&apos;t really change this decision (the specs alone pointed here), so <b>the whole commission returns to the merchant</b>.
                  </p>
                )}
                <ul className="mt-4 divide-y divide-line">
                  {(o.attributions ?? []).map((r) => (
                    <li key={r.source_id} className="py-3">
                      <div className="flex items-start gap-3">
                        <SourceIcon type={r.source_type} />
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-center gap-2">
                            <Link href={`/creator/?id=${r.creator_id}`} className="font-medium hover:underline">
                              {r.creator_name}
                            </Link>
                            <span className="truncate text-sm text-muted">{r.source_title}</span>
                          </div>
                          <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
                            <span className="tabular">
                              φ = {(r.phi * 100).toFixed(1)} pts <span title="Uncertainty from sampling">± {(r.phi_stddev * 100).toFixed(1)}</span>
                            </span>
                            {r.weight > 0 && <span className="tabular">{pct(r.weight)} of the creator pool</span>}
                            <span>{r.reason}</span>
                          </div>
                          {r.explanation && <p className="mt-1.5 text-sm text-ink-2">{r.explanation}</p>}
                        </div>
                        <div className="text-right">
                          <div className={`tabular font-semibold ${r.amount_cents > 0 ? "" : "text-muted"}`}>{usd(r.amount_cents)}</div>
                          <StatusPill status={r.status} className="mt-1" />
                        </div>
                      </div>
                    </li>
                  ))}
                </ul>
              </>
            )}
          </section>
          {job?.status === "DONE" && <CoalitionExplorer job={job} rows={o.attributions ?? []} productNames={productNames} />}
        </div>

        <aside className="min-w-0 space-y-6">
          <section className="card p-5" aria-labelledby="money">
            <h2 id="money" className="text-lg font-semibold tracking-tight">
              Commission
            </h2>
            <p className="mb-4 mt-1 text-xs text-muted">{usd(o.split.commission_cents)} reserved at capture, split by influence.</p>
            <SplitBar o={o} />
            <div className="mt-4 rounded-xl bg-paper-2 p-3 text-sm">
              {o.vested_at ? (
                <span className="text-paid">✓ Vested: the return window passed with no refund or dispute.</span>
              ) : o.dispute_status === "OPEN" ? (
                <span className="text-frozen">Frozen while a dispute is open.</span>
              ) : o.status === "REFUNDED" ? (
                <span className="text-forfeit">Refunded: pending commission forfeited.</span>
              ) : o.vests_at ? (
                vestsIn > 0 ? (
                  <span>
                    Vests in <b className="tabular">{dur(vestsIn)}</b> if the buyer keeps it.
                  </span>
                ) : (
                  <span>Return window passed: vesting on the next tick…</span>
                )
              ) : (
                <span className="text-muted">Awaiting capture.</span>
              )}
            </div>
          </section>

          <section className="card p-5 text-sm" aria-labelledby="pp">
            <h2 id="pp" className="text-lg font-semibold tracking-tight">
              Payment
            </h2>
            {o.seeded && <p className="mt-3 text-muted">Seeded history: no PayPal objects exist for this order.</p>}
            <dl className="mt-3 space-y-2">
              {o.seeded ? null : (
                <>
                  <div className="flex justify-between gap-2">
                    <dt className="text-muted">PayPal order</dt>
                    <dd className="truncate font-mono text-xs">{o.paypal_order_id}</dd>
                  </div>
                  <div className="flex justify-between gap-2">
                    <dt className="text-muted">Capture</dt>
                    <dd className="truncate font-mono text-xs">{o.paypal_capture_id ?? "—"}</dd>
                  </div>
                </>
              )}
              {(o.refunds ?? []).map((r) => (
                <div key={r.paypal_refund_id} className="flex justify-between gap-2">
                  <dt className="text-forfeit">Refund {usd(r.amount_cents)}</dt>
                  <dd className="truncate font-mono text-xs">{r.paypal_refund_id}</dd>
                </div>
              ))}
            </dl>
            {o.request && (
              <div className="mt-4 border-t border-line pt-3">
                <div className="text-xs text-muted">Shopper asked</div>
                <div className="mt-0.5">“{o.request}”</div>
              </div>
            )}
          </section>

          {byCreator.size > 0 && (
            <section className="card p-5" aria-label="Creators paid">
              <div className="eyebrow mb-3">Creators on this order</div>
              <ul className="space-y-2">
                {[...byCreator.entries()].map(([cid, amt]) => {
                  const r = o.attributions!.find((x) => x.creator_id === cid)!;
                  return (
                    <li key={cid}>
                      <Link href={`/creator/?id=${cid}`} className="flex items-center gap-3 rounded-lg p-1 hover:bg-paper-2">
                        <Avatar name={r.creator_name} hue={r.avatar_hue} size={28} />
                        <span className="flex-1 text-sm">{r.creator_name}</span>
                        <span className={`tabular text-sm font-semibold ${amt ? "" : "text-muted"}`}>{usd(amt)}</span>
                      </Link>
                    </li>
                  );
                })}
              </ul>
            </section>
          )}
        </aside>
      </div>
    </div>
  );
}

function OrderPageInner() {
  const id = useSearchParams().get("id");
  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6">
      {id ? <OrderView id={id} /> : <Empty title="No order selected">Buy something on the shopper page first.</Empty>}
    </div>
  );
}

export default function OrderPage() {
  return (
    <Suspense fallback={<div className="mx-auto max-w-7xl px-4 py-8"><Spinner /></div>}>
      <OrderPageInner />
    </Suspense>
  );
}

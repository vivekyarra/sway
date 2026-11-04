"use client";

import type { ColDef, ICellRendererParams } from "ag-grid-community";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect, useMemo, useState } from "react";
import { Grid, sparkline } from "@/components/Grid";
import { useConfig } from "@/components/Providers";
import { Badge, ErrorBox, Spinner, Stat, StatusPill } from "@/components/ui";
import { api } from "@/lib/api";
import { usePoll } from "@/lib/hooks";
import { ago, dur, pct, shortDate, usd } from "@/lib/format";
import { completeStep } from "@/lib/tour";
import type { LedgerEntry, Merchant, PayoutItem } from "@/lib/types";

type OrderRow = Merchant["orders"][number];
type ContentRow = Merchant["content"][number];
type Webhook = NonNullable<Merchant["webhooks"]>[number];

const TABS = [
  { key: "content", label: "Content performance" },
  { key: "orders", label: "Orders" },
  { key: "payouts", label: "Payouts" },
  { key: "ledger", label: "Ledger" },
  { key: "webhooks", label: "Webhooks" },
] as const;

function Trend({ m }: { m: Merchant }) {
  const max = Math.max(1, ...m.daily.map((d) => d.revenue_cents));
  const total = m.daily.reduce((a, d) => a + d.revenue_cents, 0);
  const earned = m.daily.reduce((a, d) => a + d.commission_cents, 0);
  return (
    <div className="card p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="eyebrow">Agent-driven revenue · 14 days</div>
        <div className="text-xs text-muted">
          {usd(total)} revenue · <span className="font-semibold text-accent-ink">{usd(earned)}</span> earned by creators
        </div>
      </div>
      <div className="mt-3 flex h-28 items-end gap-1.5" role="img" aria-label={`Daily agent-driven revenue, ${usd(total)} over 14 days`}>
        {m.daily.map((d) => (
          <div key={d.day} className="flex h-full flex-1 flex-col justify-end" title={`${d.day}: ${usd(d.revenue_cents)} revenue, ${usd(d.commission_cents)} to creators`}>
            <div className="w-full rounded-t-md bg-accent/80 transition-all hover:bg-accent" style={{ height: `${(d.revenue_cents / max) * 100}%`, minHeight: d.revenue_cents ? 4 : 1, opacity: d.revenue_cents ? 1 : 0.25 }} />
          </div>
        ))}
      </div>
      <div className="mt-1 flex justify-between text-[10px] text-muted">
        <span>{m.daily[0]?.day}</span>
        <span>today</span>
      </div>
    </div>
  );
}

function OrderActions({ row, onDone }: { row: OrderRow; onDone: (msg: string) => void }) {
  const cfg = useConfig();
  const [busy, setBusy] = useState(false);
  const refundable = row.status === "CAPTURED" || row.status === "PARTIALLY_REFUNDED";
  const act = async (fn: () => Promise<unknown>, msg: string) => {
    setBusy(true);
    try {
      await fn();
      onDone(msg);
    } catch (e) {
      onDone((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="flex h-full items-center gap-1.5">
      <button
        className="btn btn-ghost btn-sm py-1 text-forfeit"
        disabled={!refundable || busy}
        onClick={() => confirm(`Refund ${row.product_name} (${usd(row.amount_cents - row.refunded_cents)})?`) && act(async () => {
            await api.post(`/merchant/orders/${row.id}/refund`, { reason: "merchant refund" });
            completeStep(7);
          }, `Refunded ${row.product_name}. Pending commission forfeited.`)}
      >
        Refund
      </button>
      {cfg?.demo_mode && (
        <select
          aria-label="Simulate dispute"
          className="rounded-full border border-line-2 bg-card px-2 py-1 text-xs"
          disabled={busy || row.status === "REFUNDED"}
          value=""
          onChange={(e) => {
            const action = e.target.value;
            if (action) act(() => api.post(`/merchant/orders/${row.id}/simulate-dispute`, { action }), `Simulated dispute: ${action.replace("_", " ")}.`);
          }}
        >
          <option value="">Simulate dispute…</option>
          {row.dispute_status !== "OPEN" && <option value="open">Open dispute</option>}
          {row.dispute_status === "OPEN" && <option value="resolve_seller">Resolve for seller</option>}
          {row.dispute_status === "OPEN" && <option value="resolve_buyer">Resolve for buyer</option>}
        </select>
      )}
      {busy && <Spinner />}
    </div>
  );
}

function MerchantView() {
  const cfg = useConfig();
  const params = useSearchParams();
  const router = useRouter();
  const tab = (params.get("tab") as (typeof TABS)[number]["key"]) ?? "orders";
  const { data: m, error, refresh } = usePoll<Merchant>("/merchant/overview", 3000);
  const [filter, setFilter] = useState("");
  const [toast, setToast] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    if (tab === "ledger" && m?.verification.ok) completeStep(8);
  }, [tab, m?.verification.ok]);
  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(null), 5000);
    return () => clearTimeout(t);
  }, [toast]);

  const note = (msg: string) => {
    setToast(msg);
    refresh();
  };

  const orderCols = useMemo<ColDef<OrderRow>[]>(
    () => [
      {
        field: "id",
        headerName: "Order",
        minWidth: 150,
        cellRenderer: (p: ICellRendererParams<OrderRow>) => (
          <Link className="link font-mono text-xs" href={`/order/?id=${p.value}`}>
            {p.value}
          </Link>
        ),
      },
      { field: "product_name", headerName: "Product", minWidth: 130 },
      { field: "amount_cents", headerName: "Amount", valueFormatter: (p) => usd(p.value), maxWidth: 110, cellClass: "tabular" },
      { field: "status", headerName: "Capture", cellRenderer: (p: ICellRendererParams<OrderRow>) => <StatusPill status={p.value} />, minWidth: 130 },
      {
        field: "dispute_status",
        headerName: "Dispute",
        minWidth: 150,
        cellRenderer: (p: ICellRendererParams<OrderRow>) =>
          p.value === "NONE" ? <span className="text-muted">—</span> : (
            <span className="flex items-center gap-1"><StatusPill status={p.value === "OPEN" ? "OPEN" : p.value} />{p.data?.dispute_simulated && <Badge tone="sim">sim</Badge>}</span>
          ),
      },
      { field: "attribution_status", headerName: "Attribution", cellRenderer: (p: ICellRendererParams<OrderRow>) => <StatusPill status={p.value} />, maxWidth: 130 },
      { field: "share", headerName: "Influence", valueFormatter: (p) => pct(p.value), maxWidth: 110, cellClass: "tabular" },
      { field: "creator_pool_cents", headerName: "Creator pool", valueFormatter: (p) => usd(p.value), maxWidth: 125, cellClass: "tabular" },
      { field: "fee_cents", headerName: "Sway fee", valueFormatter: (p) => usd(p.value), maxWidth: 110, cellClass: "tabular" },
      { field: "returned_to_merchant_cents", headerName: "Returned", valueFormatter: (p) => usd(p.value), maxWidth: 110, cellClass: "tabular" },
      { field: "balanced", headerName: "Balances", maxWidth: 105, cellRenderer: (p: ICellRendererParams<OrderRow>) => (p.value ? <span className="text-paid">✓</span> : <span className="text-forfeit">✕</span>) },
      {
        field: "payment_mode",
        headerName: "Origin",
        maxWidth: 110,
        valueFormatter: (p) => (p.data?.seeded ? "seeded" : p.value === "sim" ? "simulator" : "sandbox"),
      },
      { headerName: "Actions", minWidth: 270, sortable: false, filter: false, cellRenderer: (p: ICellRendererParams<OrderRow>) => p.data && <OrderActions row={p.data} onDone={note} /> },
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );

  const contentCols = useMemo<ColDef<ContentRow>[]>(
    () => [
      { field: "title", headerName: "Source", minWidth: 220, tooltipField: "title" },
      { field: "creator_name", headerName: "Creator", minWidth: 130 },
      { field: "status", headerName: "Screening", maxWidth: 125, cellRenderer: (p: ICellRendererParams<ContentRow>) => <span title={p.data?.status_reason}><StatusPill status={p.value} /></span> },
      { field: "purchases_influenced", headerName: "Purchases", maxWidth: 115, cellClass: "tabular" },
      { field: "avg_contribution", headerName: "Avg contribution", valueFormatter: (p) => `${(p.value ?? 0).toFixed(1)} pts`, maxWidth: 145, cellClass: "tabular" },
      { field: "spark", headerName: "φ trend", sortable: false, filter: false, maxWidth: 120, cellRenderer: (p: ICellRendererParams<ContentRow>) => <span className="flex h-full items-center" dangerouslySetInnerHTML={{ __html: sparkline(p.value) }} /> },
      { field: "revenue_influenced_cents", headerName: "Revenue influenced", valueFormatter: (p) => usd(p.value), maxWidth: 160, cellClass: "tabular" },
      { field: "return_rate", headerName: "Return rate", valueFormatter: (p) => pct(p.value), maxWidth: 120, cellClass: "tabular" },
      { field: "commission_earned_cents", headerName: "Earned", valueFormatter: (p) => usd(p.value), maxWidth: 110, cellClass: "tabular" },
      { field: "commission_vested_cents", headerName: "Vested", valueFormatter: (p) => usd(p.value), maxWidth: 110, cellClass: "tabular" },
    ],
    [],
  );

  const payoutCols = useMemo<ColDef<PayoutItem>[]>(
    () => [
      { field: "created_at", headerName: "When", valueFormatter: (p) => ago(p.value), maxWidth: 120 },
      { field: "creator_name", headerName: "Creator" },
      { field: "receiver", headerName: "PayPal receiver", minWidth: 200 },
      { field: "amount_cents", headerName: "Amount", valueFormatter: (p) => usd(p.value), maxWidth: 110, cellClass: "tabular" },
      { field: "offset_cents", headerName: "Clawback offset", valueFormatter: (p) => (p.value ? usd(p.value) : "—"), maxWidth: 140, cellClass: "tabular" },
      { field: "status", headerName: "Status", cellRenderer: (p: ICellRendererParams<PayoutItem>) => <StatusPill status={p.value} />, maxWidth: 130 },
      { field: "paypal_payout_batch_id", headerName: "PayPal batch", valueFormatter: (p) => (p.data?.mode === "seeded" ? "seeded history" : p.value ?? "submitting…"), minWidth: 170, cellClass: "font-mono text-xs" },
      { field: "paypal_payout_item_id", headerName: "PayPal item", valueFormatter: (p) => (p.data?.mode === "seeded" ? "—" : p.value ?? "…"), minWidth: 170, cellClass: "font-mono text-xs" },
    ],
    [],
  );

  const ledgerCols = useMemo<ColDef<LedgerEntry>[]>(
    () => [
      { field: "seq", headerName: "#", maxWidth: 80, sort: "desc", cellClass: "tabular" },
      { field: "type", headerName: "Type", minWidth: 190, cellClass: "font-mono text-xs" },
      { field: "signed_cents", headerName: "Amount", maxWidth: 120, valueFormatter: (p) => (p.value === 0 ? `(${usd(p.data?.amount_cents)})` : usd(p.value)), cellClass: "tabular" },
      { field: "from_account", headerName: "From", cellClass: "font-mono text-xs", minWidth: 160 },
      { field: "to_account", headerName: "To", cellClass: "font-mono text-xs", minWidth: 160 },
      { field: "order_id", headerName: "Order", cellClass: "font-mono text-xs", minWidth: 150 },
      { field: "creator_id", headerName: "Creator", cellClass: "font-mono text-xs", maxWidth: 130 },
      { field: "memo", headerName: "Memo", minWidth: 200, tooltipField: "memo" },
      { field: "hash", headerName: "Hash", valueFormatter: (p) => `${String(p.value).slice(0, 10)}…`, cellClass: "font-mono text-xs", maxWidth: 130, tooltipField: "hash" },
    ],
    [],
  );

  const webhookCols = useMemo<ColDef<Webhook>[]>(
    () => [
      { field: "received_at", headerName: "Received", valueFormatter: (p) => ago(p.value), maxWidth: 120 },
      { field: "event_type", headerName: "Event", minWidth: 230, cellClass: "font-mono text-xs" },
      { field: "resource_id", headerName: "Resource", cellClass: "font-mono text-xs", minWidth: 160 },
      {
        field: "verified",
        headerName: "Signature",
        maxWidth: 140,
        cellRenderer: (p: ICellRendererParams<Webhook>) => (p.data?.simulated ? <Badge tone="sim">SIMULATED</Badge> : p.value ? <Badge tone="good">verified</Badge> : <Badge tone="warn">unverified</Badge>),
      },
      { field: "outcome", headerName: "Outcome", minWidth: 200 },
      { field: "processed_at", headerName: "Processed", valueFormatter: (p) => (p.value ? ago(p.value) : "queued"), maxWidth: 120 },
    ],
    [],
  );

  if (error) return <ErrorBox message={error.message} onRetry={refresh} />;
  if (!m) return <div className="skeleton h-[600px]" />;
  const k = m.kpis;

  async function action(name: string, path: string, done: (r: Record<string, number>) => string) {
    setBusy(name);
    try {
      const r = await api.post<Record<string, number>>(path);
      note(done(r));
    } catch (e) {
      note((e as Error).message);
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <div className="eyebrow">Merchant</div>
          <h1 className="display mt-1 text-4xl">{m.name}</h1>
          <div className="mt-2 flex flex-wrap gap-2 text-sm">
            <Badge>Commission {(m.commission_rate_bps / 100).toFixed(0)}% of order</Badge>
            <Badge>Vesting window {dur(m.vesting_window_seconds)}</Badge>
            <Badge title="Read-only: Sway's cut of the creator pool">Sway fee {(m.sway_fee_bps / 100).toFixed(0)}% of creator pool</Badge>
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          {cfg?.demo_mode && (
            <button className="btn btn-ghost" disabled={!!busy} onClick={() => action("ff", "/demo/fast-forward", (r) => `Fast-forwarded vesting: ${r.vested ?? 0} order(s) vested. Payouts follow automatically.`)}>
              {busy === "ff" ? <Spinner /> : "⏩"} Fast-forward vesting
            </button>
          )}
          <button
            className="btn btn-primary"
            disabled={!!busy}
            onClick={() => action("pay", "/merchant/payouts/run", (r) => (r.items ? `Payout batch created: ${r.items} creator(s), ${usd(r.total_cents)}.` : "Nothing vested to pay out right now."))}
          >
            {busy === "pay" ? <Spinner /> : null} Run payouts now
          </button>
        </div>
      </div>

      {toast && (
        <div className="rise rounded-xl bg-ink px-4 py-3 text-sm text-white" role="status">
          {toast}
        </div>
      )}

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
        <Stat label="Agent-driven revenue" value={usd(k.agent_revenue_cents)} sub={`${k.orders} orders, net of refunds`} />
        <Stat label="Commission paid" value={usd(k.commission_paid_cents)} tone="paid" sub={`${usd(k.commission_pending_cents)} still pending or vested`} />
        <Stat label="Commission returned" value={usd(k.commission_returned_cents)} tone="vested" sub={`avg influence share ${pct(k.avg_influence_share)}`} />
        <Stat label="Forfeited" value={usd(k.commission_forfeited_cents)} tone="forfeit" sub="returns and lost disputes" />
        <Stat label="Influenced return rate" value={pct(k.influenced_return_rate)} tone="pending" sub="you only pay on kept sales" />
      </div>

      <div className="grid gap-3 lg:grid-cols-[1.4fr_1fr]">
        <Trend m={m} />
        <div className="card flex flex-col justify-between p-4">
          <div>
            <div className="eyebrow">Books</div>
            <div className={`mt-1 text-xl font-semibold ${m.verification.ok ? "text-paid" : "text-forfeit"}`}>
              {m.verification.ok ? "✓ Ledger verified" : `✕ Chain broken at #${m.verification.broken_at_seq}`}
            </div>
            <p className="mt-1 text-sm text-muted">
              {m.verification.entries} double-entry entries, SHA-256 hash-chained, append-only. Trial balance {m.verification.trial_balance_zero ? "is zero" : "is NOT zero"}. Every order: creator pool + fee + returned = commission.
            </p>
          </div>
          <div className="mt-3 truncate font-mono text-[11px] text-muted" title={m.verification.head}>
            head {m.verification.head}
          </div>
        </div>
      </div>

      <section className="card p-4 sm:p-5">
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <div role="tablist" className="flex flex-wrap gap-1 rounded-full bg-paper-2 p-1">
            {TABS.map((t) => (
              <button
                key={t.key}
                role="tab"
                aria-selected={tab === t.key}
                className={`rounded-full px-3.5 py-1.5 text-sm font-medium ${tab === t.key ? "bg-card shadow-[var(--shadow)]" : "text-ink-2 hover:text-ink"}`}
                onClick={() => router.replace(`/merchant/?tab=${t.key}`, { scroll: false })}
              >
                {t.label}
              </button>
            ))}
          </div>
          <input
            type="search"
            placeholder="Filter…"
            aria-label="Filter rows"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="ml-auto w-48 rounded-full bg-paper px-4 py-1.5 text-sm ring-1 ring-line focus:outline-none focus:ring-2 focus:ring-accent"
          />
        </div>
        {tab === "orders" && <Grid<OrderRow> rows={m.orders} cols={orderCols} quickFilter={filter} height={520} />}
        {tab === "content" && <Grid<ContentRow> rows={m.content} cols={contentCols} quickFilter={filter} height={520} />}
        {tab === "payouts" && (
          <>
            <div className="mb-3 flex flex-wrap gap-2 text-xs">
              {(m.batches ?? []).slice(0, 6).map((b) => (
                <span key={b.id} className="chip border border-line bg-paper py-1 text-ink-2">
                  {b.mode === "seeded" ? "seeded" : b.paypal_payout_batch_id ?? b.sender_batch_id} · {usd(b.total_cents)} · <StatusPill status={b.status} />
                </span>
              ))}
            </div>
            <Grid<PayoutItem> rows={m.payouts ?? []} cols={payoutCols} quickFilter={filter} height={480} />
          </>
        )}
        {tab === "ledger" && (
          <>
            <p className="mb-3 text-sm text-muted">Append-only: there are no edit or delete actions, and the database rejects UPDATE, DELETE and TRUNCATE on this table.</p>
            <Grid<LedgerEntry> rows={m.ledger ?? []} cols={ledgerCols} quickFilter={filter} height={520} />
          </>
        )}
        {tab === "webhooks" && <Grid<Webhook> rows={m.webhooks ?? []} cols={webhookCols} quickFilter={filter} height={480} />}
        <div className="mt-2 text-right text-[11px] text-muted">Updated {shortDate(new Date().toISOString())} · refreshes every 3s</div>
      </section>
    </div>
  );
}

export default function MerchantPage() {
  return (
    <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6">
      <Suspense fallback={<Spinner />}>
        <MerchantView />
      </Suspense>
    </div>
  );
}

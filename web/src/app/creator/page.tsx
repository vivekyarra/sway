"use client";

import type { ColDef, ICellRendererParams } from "ag-grid-community";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect, useMemo, useState } from "react";
import { Grid } from "@/components/Grid";
import { Avatar, Badge, Empty, ErrorBox, SourceIcon, Spinner, Stat, StatusPill } from "@/components/ui";
import { usePoll } from "@/lib/hooks";
import { pct, shortDate, usd } from "@/lib/format";
import { completeStep } from "@/lib/tour";
import type { Creator, CreatorEvent, CreatorSummary } from "@/lib/types";

function CreatorView({ id }: { id: string }) {
  const router = useRouter();
  const { data: c, error, refresh } = usePoll<Creator>(`/creators/${id}`, 3000);
  const { data: list } = usePoll<{ creators: CreatorSummary[] }>("/creators", 0);
  const [selected, setSelected] = useState<CreatorEvent | null>(null);

  useEffect(() => {
    if (c && id === "c_priya" && (c.payouts ?? []).some((p) => p.mode !== "seeded" && !["FAILED", "RETURNED", "DENIED", "BLOCKED"].includes(p.status))) completeStep(6);
  }, [c, id]);

  const cols = useMemo<ColDef<CreatorEvent>[]>(
    () => [
      { field: "date", headerName: "Date", valueFormatter: (p) => shortDate(p.value), maxWidth: 110, sort: "desc" },
      { field: "product_name", headerName: "Product", minWidth: 130 },
      { field: "source_title", headerName: "Source", minWidth: 200, tooltipField: "source_title" },
      {
        field: "phi",
        headerName: "Contribution",
        maxWidth: 150,
        valueFormatter: (p) => `${((p.value ?? 0) * 100).toFixed(1)} ± ${((p.data?.phi_stddev ?? 0) * 100).toFixed(1)} pts`,
        cellClass: "tabular",
      },
      { field: "amount_cents", headerName: "Amount", maxWidth: 110, valueFormatter: (p) => usd(p.value), cellClass: "tabular font-semibold" },
      {
        field: "status",
        headerName: "Status",
        maxWidth: 140,
        cellRenderer: (p: ICellRendererParams<CreatorEvent>) => <StatusPill status={p.value} />,
      },
      {
        colId: "origin",
        headerName: "Origin",
        maxWidth: 120,
        valueGetter: (p) => (p.data?.seeded ? "seeded" : p.data?.simulated ? "simulator" : "sandbox"),
      },
    ],
    [],
  );

  if (error) return <ErrorBox message={error.status === 404 ? "Creator not found." : error.message} onRetry={refresh} />;
  if (!c) return <div className="skeleton h-96" />;
  const b = c.balances;
  const events = c.events ?? [];

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center gap-4">
        <Avatar name={c.display_name} hue={c.avatar_hue} size={64} />
        <div className="min-w-0 flex-1">
          <div className="eyebrow">Creator</div>
          <h1 className="display text-4xl">{c.display_name}</h1>
          <div className="mt-1 flex flex-wrap items-center gap-2 text-sm text-muted">
            <span>{c.bio}</span>
            <span>·</span>
            <span className="font-mono text-xs">{c.paypal_email}</span>
            {c.verified ? <Badge tone="good">PayPal verified</Badge> : <Badge>payouts by email</Badge>}
          </div>
        </div>
        <label className="text-sm">
          <span className="sr-only">Switch creator</span>
          <select
            className="rounded-full border border-line bg-card px-3 py-2 text-sm"
            value={id}
            onChange={(e) => router.push(`/creator/?id=${e.target.value}`)}
          >
            {(list?.creators ?? [{ id, display_name: c.display_name } as CreatorSummary]).map((x) => (
              <option key={x.id} value={x.id}>
                {x.display_name}
              </option>
            ))}
          </select>
        </label>
      </div>

      <p className="display text-2xl text-ink-2 sm:text-3xl">{c.headline}</p>

      <div className="grid grid-cols-2 gap-3 md:grid-cols-5">
        <Stat label="Pending" value={usd(b.pending_cents + b.frozen_cents)} tone="pending" sub={b.frozen_cents ? `${usd(b.frozen_cents)} frozen by a dispute` : "inside the return window"} />
        <Stat label="Vested" value={usd(b.vested_cents)} tone="vested" sub="kept by the buyer; payout next" />
        <Stat label="Paid" value={usd(b.paid_cents)} tone="paid" sub="sent with PayPal Payouts" />
        <Stat label="Forfeited" value={usd(b.forfeited_cents)} tone="forfeit" sub="purchases that were returned" />
        <Stat label="Balance owed" value={usd(b.owed_cents)} tone={b.owed_cents ? "forfeit" : undefined} sub={b.owed_cents ? "netted against the next payout" : "no clawbacks outstanding"} />
      </div>

      <section className="card p-5 sm:p-6">
        <div className="mb-3 flex flex-wrap items-end justify-between gap-2">
          <div>
            <h2 className="text-lg font-semibold tracking-tight">Influence events</h2>
            <p className="text-sm text-muted">Every purchase your writing took part in. Click a row to see why it mattered.</p>
          </div>
          <span className="text-xs text-muted">{events.length} events</span>
        </div>
        {events.length ? (
          <Grid<CreatorEvent>
            rows={events}
            cols={cols}
            height={Math.min(460, 90 + events.length * 42)}
            options={{ rowSelection: { mode: "singleRow", checkboxes: false, enableClickSelection: true }, onRowClicked: (e) => setSelected(e.data ?? null) }}
          />
        ) : (
          <Empty title="No influence yet">When an agent buys something your writing helped decide, it shows up here.</Empty>
        )}
        {selected && (
          <div className="rise mt-4 rounded-2xl border border-line bg-paper p-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="font-semibold">
                {selected.product_name} · {selected.source_title}
              </div>
              <Link className="link text-sm" href={`/order/?id=${selected.order_id}`}>
                Open order →
              </Link>
            </div>
            <p className="mt-2 text-[15px] leading-relaxed text-ink-2">{selected.explanation ?? "Explanation is being written…"}</p>
            <div className="mt-2 flex flex-wrap gap-3 text-xs text-muted">
              <span>Causal contribution {(selected.phi * 100).toFixed(1)} ± {(selected.phi_stddev * 100).toFixed(1)} pts</span>
              <span>{pct(selected.contribution_pct / 100)} of the creator pool</span>
              <span>{usd(selected.amount_cents)} · {selected.status}</span>
              <span>{selected.reason}</span>
            </div>
          </div>
        )}
      </section>

      <div className="grid gap-6 lg:grid-cols-2">
        <section className="card p-5">
          <h2 className="mb-3 text-lg font-semibold tracking-tight">Your sources</h2>
          <ul className="divide-y divide-line">
            {(c.sources ?? []).map((s) => (
              <li key={s.id} className="flex items-start gap-3 py-3">
                <SourceIcon type={s.type} />
                <div className="min-w-0 flex-1">
                  <div className="font-medium">{s.title}</div>
                  <div className="text-xs text-muted">
                    Published {s.published_at} · {s.purchases_influenced} purchases influenced · return rate {pct(s.return_rate)} · earned {usd(s.earned_cents)}
                  </div>
                  {s.status !== "active" && <div className="mt-1 text-xs font-medium text-forfeit">{s.status_reason}</div>}
                </div>
                <StatusPill status={s.status} />
              </li>
            ))}
          </ul>
        </section>
        <section className="card p-5">
          <h2 className="mb-3 text-lg font-semibold tracking-tight">Payouts</h2>
          {(c.payouts ?? []).length === 0 ? (
            <Empty title="No payouts yet">Vested earnings above the minimum are paid automatically.</Empty>
          ) : (
            <ul className="divide-y divide-line">
              {c.payouts!.map((p) => (
                <li key={p.id} className="flex items-center gap-3 py-3 text-sm">
                  <div className="min-w-0 flex-1">
                    <div className="tabular font-semibold">
                      {usd(p.amount_cents)} {p.offset_cents > 0 && <span className="text-xs font-normal text-forfeit">(after {usd(p.offset_cents)} clawback offset)</span>}
                    </div>
                    <div className="truncate font-mono text-[11px] text-muted">
                      {p.mode === "seeded" ? "seeded history" : `PayPal batch ${p.paypal_payout_batch_id ?? "pending"} · item ${p.paypal_payout_item_id ?? "…"}`}
                    </div>
                  </div>
                  <span className="text-xs text-muted">{shortDate(p.created_at)}</span>
                  <StatusPill status={p.status} />
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </div>
  );
}

function CreatorsIndex() {
  const { data, error } = usePoll<{ creators: CreatorSummary[] }>("/creators", 5000);
  if (error) return <ErrorBox message={error.message} />;
  return (
    <div>
      <div className="eyebrow">Creators</div>
      <h1 className="display mt-1 text-4xl">The people agents learn from</h1>
      <div className="mt-6 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {(data?.creators ?? []).map((c) => (
          <Link key={c.id} href={`/creator/?id=${c.id}`} className="card flex items-center gap-3 p-4 hover:shadow-[var(--shadow-lg)]">
            <Avatar name={c.display_name} hue={c.avatar_hue} size={44} />
            <div className="min-w-0">
              <div className="font-semibold">{c.display_name}</div>
              <div className="text-xs text-muted">
                {usd(c.earned_cents)} earned · {usd(c.paid_cents)} paid
              </div>
              {c.flagged_sources > 0 && <div className="text-xs text-forfeit">{c.flagged_sources} source flagged</div>}
            </div>
          </Link>
        ))}
        {!data && [0, 1, 2, 3].map((i) => <div key={i} className="skeleton h-20" />)}
      </div>
    </div>
  );
}

function Inner() {
  const id = useSearchParams().get("id");
  return <div className="mx-auto max-w-7xl px-4 py-8 sm:px-6">{id ? <CreatorView key={id} id={id} /> : <CreatorsIndex />}</div>;
}

export default function CreatorPage() {
  return (
    <Suspense fallback={<div className="mx-auto max-w-7xl px-4 py-8"><Spinner /></div>}>
      <Inner />
    </Suspense>
  );
}

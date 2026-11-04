"use client";

import { PayPalButtons, PayPalScriptProvider } from "@paypal/react-paypal-js";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { api } from "@/lib/api";
import { usd } from "@/lib/format";
import { completeStep, rememberOrder } from "@/lib/tour";
import type { Order } from "@/lib/types";
import { useConfig } from "./Providers";
import { ErrorBox, Spinner } from "./ui";

type Created = { order_id: string; paypal_order_id: string; amount_cents: number; mode: string };

export function Checkout({ decisionId, amountCents, productName }: { decisionId: string; amountCents: number; productName: string }) {
  const cfg = useConfig();
  const router = useRouter();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [simOrder, setSimOrder] = useState<Created | null>(null);

  const done = (o: Order) => {
    rememberOrder(o.id);
    completeStep(3);
    router.push(`/order/?id=${o.id}`);
  };

  if (!cfg) return <div className="skeleton h-12 w-full" />;

  if (cfg.paypal.mode === "sandbox" && cfg.paypal.client_id) {
    return (
      <div>
        <PayPalScriptProvider options={{ clientId: cfg.paypal.client_id, currency: "USD", intent: "capture", components: "buttons" }}>
          <PayPalButtons
            style={{ layout: "horizontal", shape: "pill", label: "buynow", height: 46, tagline: false }}
            forceReRender={[decisionId]}
            createOrder={async () => {
              setError(null);
              const o = await api.post<Created>("/paypal/orders", { decision_id: decisionId });
              return o.paypal_order_id;
            }}
            onApprove={async (data) => {
              setBusy(true);
              try {
                done(await api.post<Order>(`/paypal/orders/${data.orderID}/capture`));
              } catch (e) {
                setError((e as Error).message);
              } finally {
                setBusy(false);
              }
            }}
            onError={(e) => setError(String((e as unknown as Error)?.message ?? e))}
            onCancel={() => setError("Checkout cancelled. Nothing was charged.")}
          />
        </PayPalScriptProvider>
        {busy && (
          <div className="mt-2 flex items-center gap-2 text-sm text-muted">
            <Spinner /> Capturing payment…
          </div>
        )}
        {error && <div className="mt-3"><ErrorBox message={error} /></div>}
      </div>
    );
  }

  // Simulator: same API routes, same idempotent handlers; nothing real is charged and everything is labelled.
  return (
    <div>
      <button
        className="btn w-full bg-[#ffc439] py-3 text-[15px] text-[#111] hover:bg-[#f2b72c]"
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          setError(null);
          try {
            setSimOrder(await api.post<Created>("/paypal/orders", { decision_id: decisionId }));
          } catch (e) {
            setError((e as Error).message);
          } finally {
            setBusy(false);
          }
        }}
      >
        {busy && !simOrder ? <Spinner /> : null} Buy with PayPal <span className="rounded bg-black/10 px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wide">simulator</span>
      </button>
      <p className="mt-2 text-center text-xs text-muted">No PayPal keys configured, so checkout runs through Sway&apos;s PayPal simulator. Set PAYPAL_CLIENT_ID for the real sandbox.</p>
      {error && <div className="mt-3"><ErrorBox message={error} /></div>}
      {simOrder && (
        <div className="fixed inset-0 z-[70] flex items-center justify-center bg-ink/40 p-4 backdrop-blur-sm" role="dialog" aria-modal aria-label="Simulated PayPal approval">
          <div className="card rise w-full max-w-sm overflow-hidden">
            <div className="bg-[#003087] px-5 py-4 text-white">
              <div className="text-xs font-semibold uppercase tracking-widest text-white/70">PayPal simulator</div>
              <div className="mt-1 text-lg font-semibold">Approve payment</div>
            </div>
            <div className="space-y-3 p-5 text-sm">
              <div className="flex justify-between">
                <span className="text-muted">Item</span>
                <span className="font-medium">{productName}</span>
              </div>
              <div className="flex justify-between">
                <span className="text-muted">Total</span>
                <span className="tabular font-semibold">{usd(simOrder.amount_cents || amountCents)}</span>
              </div>
              <div className="flex justify-between">
                <span className="text-muted">Simulated order</span>
                <span className="font-mono text-xs">{simOrder.paypal_order_id}</span>
              </div>
              <p className="rounded-lg bg-paper-2 p-3 text-xs text-ink-2">This approval screen stands in for PayPal&apos;s popup. The order, capture, webhook and refund flows behind it are identical to sandbox mode.</p>
              <div className="flex gap-2 pt-1">
                <button className="btn btn-ghost flex-1" onClick={() => setSimOrder(null)} disabled={busy}>
                  Cancel
                </button>
                <button
                  className="btn flex-1 bg-[#0070e0] text-white hover:bg-[#005ec0]"
                  disabled={busy}
                  onClick={async () => {
                    setBusy(true);
                    try {
                      done(await api.post<Order>(`/paypal/orders/${simOrder.paypal_order_id}/capture`));
                    } catch (e) {
                      setError((e as Error).message);
                      setSimOrder(null);
                    } finally {
                      setBusy(false);
                    }
                  }}
                >
                  {busy ? <Spinner /> : null} Pay {usd(simOrder.amount_cents || amountCents)}
                </button>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useState } from "react";
import { api } from "@/lib/api";
import { dur } from "@/lib/format";
import { resetTour } from "@/lib/tour";
import { useConfig } from "./Providers";
import { Badge, Spinner } from "./ui";

export function Logo() {
  return (
    <Link href="/" className="flex items-center gap-2" aria-label="Sway home">
      <svg viewBox="0 0 32 32" className="h-7 w-7" aria-hidden>
        <rect width="32" height="32" rx="9" fill="#17141f" />
        <path d="M7 19c3-6 6-6 9 0s6 6 9 0" fill="none" stroke="#8f7bff" strokeWidth="3" strokeLinecap="round" />
        <circle cx="16" cy="11" r="2.4" fill="#fff" />
      </svg>
      <span className="text-[1.15rem] font-semibold tracking-tight">Sway</span>
    </Link>
  );
}

const roles = [
  { key: "shopper", label: "Shopper", href: "/shop/" },
  { key: "creator", label: "Creator", href: "/creator/?id=c_priya" },
  { key: "merchant", label: "Merchant", href: "/merchant/" },
];

export function TopBar() {
  const cfg = useConfig();
  const path = usePathname();
  const router = useRouter();
  const [resetting, setResetting] = useState(false);
  const role = path.startsWith("/merchant") ? "merchant" : path.startsWith("/creator") ? "creator" : path.startsWith("/shop") || path.startsWith("/order") ? "shopper" : "";

  async function reset() {
    if (!confirm("Reset all data? Orders made since the last reset are deleted and the catalog and order history are rebuilt.")) return;
    setResetting(true);
    try {
      await api.post("/admin/reset");
      resetTour();
      router.push("/merchant/");
      window.location.reload();
    } catch (e) {
      alert((e as Error).message);
    } finally {
      setResetting(false);
    }
  }

  return (
    <header className="sticky top-0 z-40 border-b border-line bg-paper/85 backdrop-blur-md">
      <div className="mx-auto flex h-14 max-w-7xl items-center gap-4 px-4 sm:px-6">
        <Logo />
        <nav aria-label="Role" className="ml-2 hidden items-center rounded-full border border-line bg-card p-0.5 sm:flex">
          {roles.map((r) => (
            <Link
              key={r.key}
              href={r.href}
              aria-current={role === r.key ? "page" : undefined}
              className={`rounded-full px-3.5 py-1.5 text-sm font-medium transition ${role === r.key ? "bg-ink text-white" : "text-ink-2 hover:bg-paper-2"}`}
            >
              {r.label}
            </Link>
          ))}
        </nav>
        <div className="ml-auto flex items-center gap-2">
          {cfg?.test_tools && (
            <Badge tone="accent" title="Commission vests after this return window if the purchase is kept">
              Vesting window {dur(cfg.vesting_window_seconds)}
            </Badge>
          )}
          {cfg && (
            <span className="hidden items-center gap-1.5 md:flex">
              <Badge tone={cfg.paypal.mode === "sandbox" ? "good" : "test"} title={cfg.paypal.mode === "sandbox" ? "PayPal Sandbox REST API" : "PayPal test mode: add PayPal credentials to use Sandbox"}>
                PayPal {cfg.paypal.mode === "sandbox" ? "Sandbox" : "Test mode"}
              </Badge>
              <Badge tone={cfg.llm.mode === "claude" ? "good" : "neutral"} title={`Decision model: ${cfg.llm.model}`}>
                Agent: {cfg.llm.mode === "claude" ? cfg.llm.model : "Sway engine"}
              </Badge>
            </span>
          )}
          {cfg?.test_tools && role === "merchant" && (
            <button className="btn btn-ghost btn-sm" onClick={reset} disabled={resetting}>
              {resetting ? <Spinner /> : null} Reset data
            </button>
          )}
        </div>
      </div>
      <nav aria-label="Role" className="flex border-t border-line sm:hidden">
        {roles.map((r) => (
          <Link key={r.key} href={r.href} className={`flex-1 py-2 text-center text-sm font-medium ${role === r.key ? "bg-ink text-white" : "text-ink-2"}`}>
            {r.label}
          </Link>
        ))}
      </nav>
    </header>
  );
}

"use client";

import { useSyncExternalStore, useState } from "react";

const noop = () => () => {};

export function ConnectAgent() {
  const origin = useSyncExternalStore(noop, () => window.location.origin, () => "https://your-sway-host");
  const [copied, setCopied] = useState<string | null>(null);
  const cli = `claude mcp add --transport http sway ${origin}/mcp`;
  const json = JSON.stringify({ mcpServers: { sway: { type: "http", url: `${origin}/mcp` } } }, null, 2);
  const copy = async (k: string, v: string) => {
    try {
      await navigator.clipboard.writeText(v);
      setCopied(k);
      setTimeout(() => setCopied(null), 1500);
    } catch {}
  };
  return (
    <section className="border-t border-line bg-ink text-white">
      <div className="mx-auto grid max-w-7xl gap-10 px-4 py-16 sm:px-6 lg:grid-cols-[1fr_1.1fr]">
        <div>
          <div className="text-[0.72rem] font-bold uppercase tracking-[0.12em] text-white/75">Bring your own agent</div>
          <h2 className="display mt-2 text-4xl sm:text-5xl">
            Any AI agent can shop <span className="italic text-[#a99bff]">through Sway.</span>
          </h2>
          <p className="mt-4 leading-relaxed text-white/75">
            Sway speaks the Model Context Protocol. Plug it into Claude or any MCP-capable agent and it gets a neutral shopper with receipts: a pick, the human sources behind it,
            counterfactuals, and a PayPal checkout link for the person. The agent never touches money, and the people it learned from get paid when the sale sticks.
          </p>
          <ul className="mt-5 grid gap-1.5 text-sm text-white/80 sm:grid-cols-2">
            {["shop_recommend", "explain_influence", "counterfactual", "order_attribution", "creator_earnings", "verify_ledger"].map((t) => (
              <li key={t} className="font-mono text-[13px]">
                <span className="text-[#a99bff]">▸</span> {t}
              </li>
            ))}
          </ul>
        </div>
        <div className="space-y-4">
          {[
            ["Claude Code", cli],
            ["Claude Desktop / any MCP client", json],
          ].map(([label, code]) => (
            <div key={label} className="overflow-hidden rounded-2xl border border-white/10 bg-white/5">
              <div className="flex items-center justify-between border-b border-white/10 px-4 py-2 text-xs text-white/60">
                <span>{label}</span>
                <button className="rounded-full px-2 py-0.5 hover:bg-white/10" onClick={() => copy(label, code)}>
                  {copied === label ? "Copied ✓" : "Copy"}
                </button>
              </div>
              <pre className="overflow-x-auto p-4 font-mono text-[12.5px] leading-6 text-white/90">{code}</pre>
            </div>
          ))}
          <p className="text-xs text-white/70">Endpoint: Streamable HTTP, JSON-RPC 2.0, MCP protocol 2025-06-18. Origin-checked and rate limited.</p>
        </div>
      </div>
    </section>
  );
}

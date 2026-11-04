"use client";

import { useMemo, useState } from "react";
import { pct } from "@/lib/format";
import type { AttributionJob, AttributionRow } from "@/lib/types";

/** Every coalition the engine evaluated, arranged by size: the raw material Shapley values are computed from. */
export function CoalitionExplorer({ job, rows, productNames }: { job: AttributionJob; rows: AttributionRow[]; productNames: Record<string, string> }) {
  const c = job.coalitions;
  const [hover, setHover] = useState<number | null>(null);
  const names = useMemo(() => {
    const m: Record<string, string> = {};
    for (const r of rows) m[r.source_id] = r.creator_name;
    return m;
  }, [rows]);
  if (!c || !c.coalitions?.length) return null;
  const players = c.players;
  const levels: typeof c.coalitions[] = [];
  for (const co of c.coalitions) {
    const size = co.members?.length ?? 0;
    (levels[size] ??= []).push(co);
  }
  const vmin = Math.min(...c.coalitions.map((x) => x.v));
  const vmax = Math.max(...c.coalitions.map((x) => x.v));
  const color = (v: number) => {
    const t = vmax > vmin ? (v - vmin) / (vmax - vmin) : 0.5;
    return `hsl(${252 - t * 4} ${40 + t * 45}% ${94 - t * 50}%)`;
  };
  const active = c.coalitions.find((x) => x.mask === hover);
  const sorted = [...rows].filter((r) => r.role === "player").sort((a, b) => b.phi - a.phi);

  return (
    <section className="card p-5 sm:p-6" aria-labelledby="coal">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 id="coal" className="text-lg font-semibold tracking-tight">
          Coalition explorer
        </h2>
        <span className="chip bg-paper-2 text-muted">
          {c.coalitions.length} coalitions × {job.method === "exact" ? "K samples" : "sampled permutations"} · {job.method === "exact" ? "exact Shapley" : "Monte Carlo"}
        </span>
      </div>
      <p className="mt-1 text-sm text-muted">
        Each tile is the agent re-deciding with only some sources. Colour is how likely it still bought this product. Hover a tile to see who was in the room.
      </p>
      <div className="mt-4 overflow-x-auto">
        <div className="flex min-w-fit flex-col-reverse gap-1.5">
          {levels.map((lvl, size) =>
            lvl ? (
              <div key={size} className="flex items-center gap-1.5">
                <span className="w-16 shrink-0 text-right text-[11px] text-muted">{size === 0 ? "no players" : `${size} source${size > 1 ? "s" : ""}`}</span>
                <div className="flex flex-wrap gap-1">
                  {lvl.map((co) => (
                    <button
                      key={co.mask}
                      onMouseEnter={() => setHover(co.mask)}
                      onFocus={() => setHover(co.mask)}
                      onMouseLeave={() => setHover(null)}
                      className={`h-7 w-7 rounded-md border transition ${hover === co.mask ? "scale-110 border-ink" : "border-white"}`}
                      style={{ background: color(co.v) }}
                      aria-label={`${(co.members ?? []).map((m) => names[m] ?? m).join(", ") || "background only"}: ${pct(co.v)}`}
                    />
                  ))}
                </div>
              </div>
            ) : null,
          )}
        </div>
      </div>
      <div className="mt-3 min-h-[44px] rounded-xl bg-paper-2 px-3 py-2 text-sm" aria-live="polite">
        {active ? (
          <>
            With <b>{(active.members ?? []).map((m) => names[m] ?? m).join(" + ") || "only background sources"}</b>: {pct(active.v)} chance of buying this product
            {active.std > 0 && <span className="text-muted"> (±{pct(active.std, 1)})</span>}; agent picks <b>{productNames[active.pick] ?? active.pick}</b>.
          </>
        ) : (
          <span className="text-muted">
            v(no sources) = {pct(c.v_none)} → v(all sources) = {pct(c.v_all)}. Shapley splits that lift fairly across {players.length} sources.
          </span>
        )}
      </div>
      <div className="mt-4">
        <div className="eyebrow mb-2">Where the lift came from</div>
        <div className="flex h-8 overflow-hidden rounded-lg bg-paper-2 text-[11px] font-semibold text-white">
          {sorted
            .filter((r) => r.phi > 0.01)
            .map((r, i) => {
              const total = sorted.filter((x) => x.phi > 0.01).reduce((a, b) => a + b.phi, 0);
              return (
                <div
                  key={r.source_id}
                  className="flex items-center justify-center truncate px-1"
                  style={{ width: `${(r.phi / total) * 100}%`, background: `hsl(${252 - i * 38} 62% ${38 - i * 3}%)` }}
                  title={`${r.creator_name}: ${pct(r.phi, 1)}`}
                >
                  {r.creator_name.split(" ")[0]} {pct(r.phi / total)}
                </div>
              );
            })}
        </div>
      </div>
      {c.interactions && players.length > 1 && (
        <details className="mt-4 text-sm">
          <summary className="cursor-pointer text-muted">Pairwise interactions (do sources reinforce or cancel each other?)</summary>
          <table className="mt-2 text-xs">
            <thead>
              <tr>
                <th />
                {players.map((p) => (
                  <th key={p} className="px-2 py-1 font-medium text-muted">
                    {(names[p] ?? p).split(" ")[0]}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {players.map((p, i) => (
                <tr key={p}>
                  <th className="px-2 py-1 text-left font-medium text-muted">{(names[p] ?? p).split(" ")[0]}</th>
                  {players.map((q, j) => {
                    const v = c.interactions![i][j];
                    return (
                      <td key={q} className="tabular px-2 py-1 text-center" style={{ background: i === j ? "transparent" : v > 0 ? `rgba(15,122,85,${Math.min(0.5, Math.abs(v) * 3)})` : `rgba(194,54,47,${Math.min(0.5, Math.abs(v) * 3)})` }}>
                        {i === j ? "·" : v.toFixed(2)}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      )}
    </section>
  );
}

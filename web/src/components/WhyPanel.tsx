"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { aspectLabel, pts, usd } from "@/lib/format";
import { completeStep } from "@/lib/tour";
import type { Decision, Influence, Rerun, Source } from "@/lib/types";
import { ErrorBox, SourceIcon, Spinner } from "./ui";

function Highlighted({ source, chosenId }: { source: Source; chosenId: string }) {
  const sentences = new Map<string, number>();
  for (const c of source.claims ?? []) {
    const sign = c.product_id === chosenId ? Math.sign(c.strength) : -Math.sign(c.strength);
    sentences.set(c.sentence, (sentences.get(c.sentence) ?? 0) + sign);
  }
  const parts = source.body.split(/(?<=[.!?])\s+/);
  return (
    <p className="text-sm leading-relaxed text-ink-2">
      {parts.map((p, i) => {
        const key = [...sentences.keys()].find((s) => p.startsWith(s.slice(0, Math.min(40, s.length))));
        const score = key ? sentences.get(key)! : 0;
        const cls = score > 0 ? "bg-paid-soft text-ink" : score < 0 ? "bg-forfeit-soft text-ink" : "";
        return (
          <span key={i} className={`rounded px-0.5 ${cls}`}>
            {p}{" "}
          </span>
        );
      })}
    </p>
  );
}

function aspectsFor(source: Source, chosenId: string): string[] {
  const out = new Set<string>();
  for (const c of source.claims ?? []) if (c.product_id === chosenId) out.add(aspectLabel[c.aspect] ?? c.aspect);
  return [...out];
}

export function WhyPanel({ decision }: { decision: Decision }) {
  const [influence, setInfluence] = useState<Influence | null>(decision.influence ?? null);
  const [loadingInf, setLoadingInf] = useState(!decision.influence);
  const [infError, setInfError] = useState<string | null>(null);
  const [excluded, setExcluded] = useState<string | null>(null);
  const [rerun, setRerun] = useState<Rerun | null>(null);
  const [rerunning, setRerunning] = useState<string | null>(null);
  const [rerunError, setRerunError] = useState<string | null>(null);
  const [open, setOpen] = useState<string | null>(null);

  useEffect(() => {
    if (decision.influence) return;
    let alive = true;
    api
      .get<{ influence: Influence }>(`/decisions/${decision.id}/influence`)
      .then((r) => alive && setInfluence(r.influence))
      .catch((e) => alive && setInfError((e as Error).message))
      .finally(() => alive && setLoadingInf(false));
    return () => {
      alive = false;
    };
  }, [decision.id, decision.influence]);

  async function remove(src: Source) {
    setRerunning(src.id);
    setRerunError(null);
    try {
      const r = await api.post<Rerun>(`/decisions/${decision.id}/rerun`, { exclude_source_ids: [src.id] });
      setRerun(r);
      setExcluded(src.id);
      if (r.flipped && decision.category === "headphones") completeStep(2);
    } catch (e) {
      setRerunError((e as Error).message);
    } finally {
      setRerunning(null);
    }
  }

  const sources = [...decision.sources].sort((a, b) => (influence?.deltas[b.id] ?? 0) - (influence?.deltas[a.id] ?? 0));
  const maxAbs = Math.max(0.05, ...Object.values(influence?.deltas ?? {}).map(Math.abs));
  const name = (id: string) => decision.candidates.find((c) => c.id === id);
  const excludedSrc = decision.sources.find((s) => s.id === excluded);

  return (
    <section aria-labelledby="why" className="card p-5 sm:p-6">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 id="why" className="text-lg font-semibold tracking-tight">
          Why I chose this
        </h2>
        <span className="chip bg-paper-2 text-muted" title="Leave-one-out: how much the agent's probability of this pick drops when one source is removed">
          influence · estimated before purchase
        </span>
      </div>
      <p className="mt-1 text-sm text-muted">
        Each bar is how much the chance of picking the {decision.chosen.name} drops without that source. Remove one to see what the agent would do instead.
      </p>

      {excludedSrc && rerun && (
        <div className={`rise mt-4 rounded-xl border p-4 ${rerun.flipped ? "border-accent/30 bg-accent-soft" : "border-line bg-paper-2"}`} aria-live="polite">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="text-[15px]">
              {rerun.flipped ? (
                <>
                  Without <b>{excludedSrc.creator_name}&apos;s {excludedSrc.type === "transcript" ? "video" : excludedSrc.type}</b>, I&apos;d pick{" "}
                  <b className="text-accent-ink">
                    {rerun.chosen.name} ({usd(rerun.chosen.price_cents)})
                  </b>
                  . That source swayed the sale.
                </>
              ) : (
                <>
                  Without <b>{excludedSrc.creator_name}&apos;s</b> source I&apos;d still pick the <b>{rerun.chosen.name}</b>.
                </>
              )}
              <span className="ml-2 text-xs text-muted">({rerun.agreement} agree)</span>
            </div>
            <button
              className="btn btn-ghost btn-sm"
              onClick={() => {
                setExcluded(null);
                setRerun(null);
              }}
            >
              ↺ Restore source
            </button>
          </div>
          {rerun.flipped && rerun.rationale && <p className="mt-2 text-sm text-ink-2">{rerun.rationale}</p>}
        </div>
      )}
      {rerunError && <div className="mt-3"><ErrorBox message={rerunError} /></div>}
      {infError && <div className="mt-3"><ErrorBox message={infError} /></div>}

      <ul className="mt-4 divide-y divide-line">
        {sources.map((s) => {
          const d = influence?.deltas[s.id];
          const pickWithout = influence?.picks_without[s.id];
          const flips = pickWithout && pickWithout !== decision.chosen.id;
          const isOut = excluded === s.id;
          const aspects = aspectsFor(s, decision.chosen.id);
          return (
            <li key={s.id} className={`py-3 transition ${isOut ? "opacity-45" : ""}`}>
              <div className="flex items-start gap-3">
                <SourceIcon type={s.type} />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <span className="font-medium">{s.creator_name}</span>
                    <span className="truncate text-sm text-muted">{s.title}</span>
                    {flips && <span className="chip bg-accent-soft text-accent-ink">pivotal</span>}
                  </div>
                  <div className="mt-2 flex items-center gap-3">
                    <div className="relative h-2.5 flex-1 rounded-full bg-paper-2" role="img" aria-label={d === undefined ? "computing" : `influence ${pts(d)}`}>
                      {d === undefined ? (
                        <div className="skeleton h-2.5 w-1/3" />
                      ) : (
                        <>
                          <span className="absolute left-1/2 top-[-3px] h-4 w-px bg-line-2" />
                          <span
                            className={`absolute top-0 h-2.5 rounded-full transition-all duration-700 ${d >= 0 ? "bg-accent" : "bg-forfeit"}`}
                            style={d >= 0 ? { left: "50%", width: `${(Math.abs(d) / maxAbs) * 50}%` } : { right: "50%", width: `${(Math.abs(d) / maxAbs) * 50}%` }}
                          />
                        </>
                      )}
                    </div>
                    <span className={`tabular w-20 text-right text-xs font-semibold ${d === undefined ? "text-muted" : d >= 0 ? "text-accent-ink" : "text-forfeit"}`}>
                      {d === undefined ? (loadingInf ? "…" : "—") : pts(d)}
                    </span>
                  </div>
                  <div className="mt-1.5 flex flex-wrap items-center gap-2 text-xs text-muted">
                    {aspects.length > 0 && <span>about {aspects.join(", ")}</span>}
                    <button className="link" onClick={() => setOpen(open === s.id ? null : s.id)} aria-expanded={open === s.id}>
                      {open === s.id ? "hide text" : "read what they wrote"}
                    </button>
                  </div>
                  {open === s.id && (
                    <div className="rise mt-2 rounded-xl bg-paper-2 p-3">
                      <Highlighted source={s} chosenId={decision.chosen.id} />
                      <div className="mt-2 text-[11px] text-muted">Green: evidence for this pick. Red: evidence against it. Published {s.published_at}.</div>
                    </div>
                  )}
                </div>
                <button className="btn btn-ghost btn-sm shrink-0" disabled={!!rerunning || isOut} onClick={() => remove(s)} aria-label={`Remove & re-run without ${s.creator_name}'s source`}>
                  {rerunning === s.id ? <Spinner /> : "Remove & re-run"}
                </button>
              </div>
            </li>
          );
        })}
      </ul>

      {(decision.ignored_sources?.length ?? 0) > 0 && (
        <div className="mt-2 rounded-xl border border-line bg-paper p-3">
          <div className="eyebrow mb-2">Screened out before the agent read anything</div>
          <ul className="space-y-2">
            {decision.ignored_sources!.map((s) => (
              <li key={s.id} className="flex items-start gap-3 text-sm">
                <SourceIcon type={s.type} />
                <div className="min-w-0">
                  <span className="font-medium">{s.creator_name}</span> <span className="text-muted">· {s.title}</span>
                  <div className="text-xs text-forfeit">{s.reason}</div>
                </div>
              </li>
            ))}
          </ul>
        </div>
      )}

      <p className="mt-4 rounded-xl bg-ink px-4 py-3 text-sm text-white/90">
        This agent earns nothing for its choice. Creators who influenced it are paid only if you keep the product.
      </p>
      {influence && influence.pick_no_sources && (
        <p className="mt-2 text-xs text-muted">
          With no human-written sources at all, the agent would pick the {name(influence.pick_no_sources)?.name ?? "—"}.
        </p>
      )}
    </section>
  );
}

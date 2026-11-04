"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";

/** Polls a GET endpoint. Pauses while the tab is hidden; `interval` can change per result (e.g. stop when done). */
export function usePoll<T>(path: string | null, interval: number | ((data: T | undefined) => number)) {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<ApiError | null>(null);
  const [tick, setTick] = useState(0);
  const dataRef = useRef<T | undefined>(undefined);
  const refresh = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    if (!path) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let cancelled = false;
    const ctrl = new AbortController();
    const run = async () => {
      if (document.visibilityState === "hidden") {
        timer = setTimeout(run, 1000);
        return;
      }
      try {
        const d = await api.get<T>(path, ctrl.signal);
        if (cancelled) return;
        dataRef.current = d;
        setData(d);
        setError(null);
      } catch (e) {
        if (cancelled || (e as Error).name === "AbortError") return;
        setError(e instanceof ApiError ? e : new ApiError(0, "error", String(e)));
      }
      const next = typeof interval === "function" ? interval(dataRef.current) : interval;
      if (!cancelled && next > 0) timer = setTimeout(run, next);
    };
    run();
    return () => {
      cancelled = true;
      ctrl.abort();
      if (timer) clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, tick]);

  return { data, error, refresh, setData };
}

/** A ticking clock for countdowns. */
export function useNow(every = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), every);
    return () => clearInterval(t);
  }, [every]);
  return now;
}

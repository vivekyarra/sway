"use client";

import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { api } from "@/lib/api";
import type { AppConfig } from "@/lib/types";

const ConfigContext = createContext<AppConfig | null>(null);

export function Providers({ children }: { children: ReactNode }) {
  const [cfg, setCfg] = useState<AppConfig | null>(null);
  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .get<AppConfig>("/config")
        .then((c) => alive && setCfg(c))
        .catch(() => alive && setTimeout(load, 2000));
    load();
    return () => {
      alive = false;
    };
  }, []);
  return <ConfigContext.Provider value={cfg}>{children}</ConfigContext.Provider>;
}

export function useConfig() {
  return useContext(ConfigContext);
}

"use client";

export type TourStep = {
  id: number;
  title: string;
  hint: string;
  href: string;
};

export const TOUR_STEPS: TourStep[] = [
  { id: 1, title: "Ask the agent for headphones", hint: "Use the suggested long-haul flights prompt.", href: "/shop/" },
  { id: 2, title: "Remove Priya's review and re-run", hint: "In “Why I chose this”, the pick flips to Brisane ANC2.", href: "/shop/" },
  { id: 3, title: "Restore it and buy with PayPal", hint: "Sandbox buyer login is below.", href: "/shop/" },
  { id: 4, title: "Watch attribution finish", hint: "Exact Shapley over every coalition of sources.", href: "/shop/" },
  { id: 5, title: "Let commission vest", hint: "Wait out the window or press Fast-forward vesting.", href: "/merchant/" },
  { id: 6, title: "See Priya's payout land", hint: "Her dashboard shows the PayPal batch ID.", href: "/creator/?id=c_priya" },
  { id: 7, title: "Refund an order, see the forfeit", hint: "Merchant → Orders → Refund.", href: "/merchant/" },
  { id: 8, title: "Confirm the ledger balances", hint: "Merchant → Ledger: hash chain verified.", href: "/merchant/?tab=ledger" },
];

const KEY = "sway.tour.v1";

export function loadTour(): number[] {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) ?? "[]");
    return Array.isArray(v) ? v.filter((x) => typeof x === "number") : [];
  } catch {
    return [];
  }
}

export function completeStep(id: number) {
  try {
    const done = new Set(loadTour());
    if (done.has(id)) return;
    done.add(id);
    localStorage.setItem(KEY, JSON.stringify([...done]));
  } catch {
    /* storage unavailable: the tour still works, it just won't persist */
  }
  window.dispatchEvent(new CustomEvent("sway:tour", { detail: id }));
}

export function resetTour() {
  try {
    localStorage.removeItem(KEY);
  } catch {}
  window.dispatchEvent(new CustomEvent("sway:tour", { detail: 0 }));
}

/** Remember orders this browser bought, so the tour can follow "your" order. */
export function rememberOrder(id: string) {
  try {
    const list: string[] = JSON.parse(localStorage.getItem("sway.orders") ?? "[]");
    if (!list.includes(id)) list.unshift(id);
    localStorage.setItem("sway.orders", JSON.stringify(list.slice(0, 20)));
  } catch {}
}

export function myOrders(): string[] {
  try {
    return JSON.parse(localStorage.getItem("sway.orders") ?? "[]");
  } catch {
    return [];
  }
}

function subscribe(cb: () => void) {
  window.addEventListener("sway:tour", cb);
  window.addEventListener("storage", cb);
  return () => {
    window.removeEventListener("sway:tour", cb);
    window.removeEventListener("storage", cb);
  };
}

/** Completed tour steps as a stable string snapshot (for useSyncExternalStore). */
export const tourStore = {
  subscribe,
  snapshot: () => {
    try {
      return localStorage.getItem(KEY) ?? "[]";
    } catch {
      return "[]";
    }
  },
  server: () => "[]",
};

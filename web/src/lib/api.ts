export class ApiError extends Error {
  status: number;
  code: string;
  suggestions?: string[];
  constructor(status: number, code: string, message: string, suggestions?: string[]) {
    super(message);
    this.status = status;
    this.code = code;
    this.suggestions = suggestions;
  }
}

function idempotencyKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

async function request<T>(method: "GET" | "POST", path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (method === "POST") {
    headers["Content-Type"] = "application/json";
    // Every write carries an Idempotency-Key, so a retried click can never double-charge or double-refund.
    headers["Idempotency-Key"] = idempotencyKey();
  }
  let res: Response;
  try {
    res = await fetch(`/api${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal, cache: "no-store" });
  } catch (e) {
    if ((e as Error).name === "AbortError") throw e;
    throw new ApiError(0, "network", "Can't reach the Sway engine. Is it running?");
  }
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    const err = (data as { error?: { code: string; message: string; suggestions?: string[] } } | null)?.error;
    if (res.status === 429) throw new ApiError(429, "rate_limited", "You're going a little fast. Try again in a few seconds.");
    throw new ApiError(res.status, err?.code ?? "error", err?.message ?? `Request failed (${res.status})`, err?.suggestions);
  }
  return data as T;
}

export const api = {
  get: <T>(path: string, signal?: AbortSignal) => request<T>("GET", path, undefined, signal),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body ?? {}),
};

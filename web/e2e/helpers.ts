import { expect, type Page, type APIRequestContext } from "@playwright/test";

export const HEADPHONES = "I need noise-cancelling headphones for long-haul flights, under $250.";

export async function ask(page: Page, request?: string) {
  await page.goto("/shop/");
  if (request) await page.getByLabel("What are you shopping for?").fill(request);
  await page.getByRole("button", { name: "Ask agent" }).click();
  await expect(page.getByText("Why I chose this")).toBeVisible();
}

/** Buys the agent's pick through the checkout UI (test mode or Sandbox buttons) and lands on the order page. */
export async function buy(page: Page) {
  await page.getByRole("button", { name: /Buy with PayPal/ }).click();
  await page.getByRole("button", { name: /^Pay \$/ }).click();
  await page.waitForURL(/\/order\/\?id=/);
  await expect(page.getByText("Coalition explorer")).toBeVisible({ timeout: 60_000 });
  return new URL(page.url()).searchParams.get("id")!;
}

export async function api<T>(request: APIRequestContext, method: "GET" | "POST", path: string, data?: unknown): Promise<T> {
  const res = method === "GET" ? await request.get(`/api${path}`) : await request.post(`/api${path}`, { data: data ?? {} });
  expect(res.ok(), `${method} ${path} -> ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()) as T;
}

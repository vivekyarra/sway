import { expect, test } from "@playwright/test";
import { api, ask, buy, HEADPHONES } from "./helpers";

type Decision = { id: string; chosen: { id: string }; ignored_sources: { id: string; reason: string }[] };
type Rerun = { flipped: boolean; chosen: { id: string } };

// E2: removing Priya's review flips the decision, across ten phrasings of the request.
test("E2 flip reproduces across 10 phrasings", async ({ request }) => {
  const phrasings = [
    HEADPHONES,
    "Noise cancelling headphones for long haul flights, budget $250",
    "Looking for ANC headphones for long-haul travel under $250",
    "Best noise-cancelling headphones for 12 hour flights under $250",
    "Headphones with noise cancelling for long flights, max $250",
    "I fly long-haul every month; need noise-cancelling headphones under $250",
    "Comfortable noise-cancelling headphones for long-haul flights below $250",
    "Noise-cancelling headphones for long-haul flights and travel, up to $250",
    "Need ANC headphones for long haul flights, less than $250",
    "Which noise-cancelling headphones should I get for long-haul flights? Under $250.",
  ];
  let flips = 0;
  for (const q of phrasings) {
    const d = await api<Decision>(request, "POST", "/agent/decide", { request: q });
    const r = await api<Rerun>(request, "POST", `/decisions/${d.id}/rerun`, { exclude_source_ids: ["s_priya_01"] });
    if (d.chosen.id === "p_halvo_q7" && r.flipped && r.chosen.id === "p_brisane_anc2") flips++;
  }
  expect(flips).toBeGreaterThanOrEqual(9);
});

// E8: injection and duplicate sources never reach the agent and show their reasons.
test("E8 manipulation is screened out with reasons", async ({ page }) => {
  await ask(page, HEADPHONES);
  await expect(page.getByText("Screened out before the agent read anything")).toBeVisible();
  await expect(page.getByText(/Blocked: tries to instruct AI agents/)).toBeVisible();
  await expect(page.getByText(/Duplicate of Priya Raman's earlier source/)).toBeVisible();
  await expect(page.getByRole("button", { name: /without DealBot/ })).toHaveCount(0);
});

// E9: when specs alone decide, the whole commission goes back to the merchant and the UI says so.
test("E9 content didn't matter returns the commission", async ({ page }) => {
  await page.goto("/shop/");
  await page.getByRole("button", { name: "A case where content doesn't matter" }).click();
  await expect(page.getByText(/The agent picked Orrin Air/)).toBeVisible();
  await buy(page);
  await expect(page.getByText(/whole commission returns to the merchant/)).toBeVisible();
});

// E4 + E6 + E7 through the merchant and order UIs.
test("E4 partial refund, E6/E7 disputes", async ({ page, request }) => {
  page.on("dialog", (d) => d.accept());
  await ask(page, "Noise-cancelling headphones for long-haul flights to Lima, under $250");
  const orderId = await buy(page);
  await page.getByRole("button", { name: "Refund 50%" }).click();
  await expect(page.getByText(/Refund issued/)).toBeVisible();
  await expect(page.getByText("Partly refunded")).toBeVisible();
  type Order = { split: { balanced: boolean }; refunded_cents: number; attributions: { amount_cents: number }[] };
  const o = await api<Order>(request, "GET", `/orders/${orderId}`);
  expect(o.refunded_cents).toBe(11450);
  expect(o.split.balanced).toBe(true);

  await api(request, "POST", `/merchant/orders/${orderId}/simulate-dispute`, { action: "open" });
  await page.goto(`/order/?id=${orderId}`);
  await expect(page.getByText("Frozen while a dispute is open.")).toBeVisible();
  await expect(page.getByText("simulated dispute")).toBeVisible();
  await api(request, "POST", `/merchant/orders/${orderId}/simulate-dispute`, { action: "resolve_buyer" });
  const verify = await api<{ ok: boolean; trial_balance_zero: boolean }>(request, "GET", "/ledger/verify");
  expect(verify.ok && verify.trial_balance_zero).toBe(true);
});

// E10: replaying a POST with the same Idempotency-Key returns the original response.
test("E10 idempotent writes", async ({ request }) => {
  const d = await api<Decision>(request, "POST", "/agent/decide", { request: HEADPHONES + " (idempotency)" });
  const headers = { "Idempotency-Key": `e2e-${Date.now()}` };
  const a = await (await request.post("/api/paypal/orders", { data: { decision_id: d.id }, headers })).json();
  const res = await request.post("/api/paypal/orders", { data: { decision_id: d.id }, headers });
  expect(res.headers()["idempotent-replayed"]).toBe("true");
  expect((await res.json()).order_id).toBe(a.order_id);
});

test("unsupported requests suggest what the agent can shop", async ({ page }) => {
  await page.goto("/shop/");
  await page.getByLabel("What are you shopping for?").fill("a pony for my birthday");
  await page.getByRole("button", { name: "Ask agent" }).click();
  await expect(page.getByText(/I can shop/)).toBeVisible();
  await expect(page.getByRole("button", { name: "headphones", exact: true })).toBeVisible();
});

test("seeded history, pretty URLs and creator dashboards", async ({ page }) => {
  await page.goto("/orders/ord_seed_01");
  await expect(page).toHaveURL(/\/order\/\?id=ord_seed_01/);
  await expect(page.getByText("seeded history").first()).toBeVisible();
  await page.goto("/creator/?id=c_dealbot");
  await expect(page.getByText(/Blocked: tries to instruct AI agents/)).toBeVisible();
  await page.goto("/creator/?id=c_ana");
  await expect(page.getByText("Balance owed")).toBeVisible();
});

test("every page fits a phone without horizontal scrolling", async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 } });
  const page = await ctx.newPage();
  for (const path of ["/", "/shop/", "/order/?id=ord_seed_01", "/creator/?id=c_priya", "/merchant/"]) {
    await page.goto(path);
    await page.waitForLoadState("networkidle");
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, `${path} overflows by ${overflow}px`).toBeLessThanOrEqual(1);
  }
  await ctx.close();
});

// An external agent shops over MCP and hands the human a checkout link that opens the same decision.
test("MCP agent handoff to PayPal checkout", async ({ page, request }) => {
  const res = await request.post("/mcp", {
    data: { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "shop_recommend", arguments: { request: "Noise-cancelling headphones for long-haul flights to Seoul, under $250" } } },
  });
  expect(res.ok()).toBeTruthy();
  const body = await res.json();
  const url = new URL(body.result.structuredContent.checkout_url);
  await page.goto(url.pathname + url.search);
  await expect(page.getByText(/Handed over by your AI agent/)).toBeVisible();
  await expect(page.getByText(/The agent picked Halvo Q7/)).toBeVisible();
  await buy(page);
});

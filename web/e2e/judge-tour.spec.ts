import { expect, test } from "@playwright/test";
import { buy } from "./helpers";

// The whole judge tour (spec 4.6), through the real UI, must tick all eight steps.
test("judge tour completes end to end", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => m.type() === "error" && errors.push(m.text()));
  page.on("dialog", (d) => d.accept());

  await page.goto("/shop/");
  await page.getByRole("button", { name: "Ask agent" }).click(); // 1
  await expect(page.getByText("Why I chose this")).toBeVisible();
  await expect(page.getByText(/The agent picked Halvo Q7/)).toBeVisible();

  await page.getByRole("button", { name: /Remove & re-run without Priya Raman/ }).click(); // 2
  await expect(page.getByText("That source swayed the sale")).toBeVisible();
  await expect(page.getByText(/I'd pick\s*Brisane ANC2/)).toBeVisible();
  await page.getByRole("button", { name: /Restore source/ }).click();

  const orderId = await buy(page); // 3 + 4
  await expect(page.getByText("Exact Shapley", { exact: true })).toBeVisible();
  await expect(page.getByText(/= \$18\.32 commission/)).toBeVisible(); // 8% of $229, split balances

  await page.goto("/merchant/");
  await page.getByRole("button", { name: /Fast-forward vesting/ }).click(); // 5
  await expect(page.getByText(/Fast-forwarded vesting/)).toBeVisible();
  await page.goto(`/order/?id=${orderId}`);
  await expect(page.getByText(/Vested: the return window passed/)).toBeVisible();

  await page.goto("/creator/?id=c_priya"); // 6
  await expect(page.getByText(/PayPal batch SIM-BATCH|PayPal batch [0-9A-Z]{8,}/).first()).toBeVisible();

  await page.goto("/shop/"); // 7
  await page.getByRole("button", { name: "Carry-on luggage" }).click();
  await expect(page.getByText("Why I chose this")).toBeVisible();
  await buy(page);
  await page.getByRole("button", { name: "Request refund" }).click();
  await expect(page.getByText(/Refund issued/)).toBeVisible();

  await page.goto("/merchant/?tab=ledger"); // 8
  await expect(page.getByText("✓ Ledger verified")).toBeVisible();

  const done = await page.evaluate(() => JSON.parse(localStorage.getItem("sway.tour.v1") || "[]"));
  expect(errors, `console errors: ${errors.join(" | ")}`).toEqual([]);
  expect(done.sort(), `tour steps done: ${JSON.stringify(done)}`).toEqual([1, 2, 3, 4, 5, 6, 7, 8]);
});

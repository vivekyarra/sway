import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";

// No serious or critical WCAG 2.1 A/AA violations on any surface.
for (const path of ["/", "/shop/", "/order/?id=ord_seed_01", "/creator/?id=c_priya", "/merchant/"]) {
  test(`accessibility: ${path}`, async ({ page }) => {
    await page.goto(path);
    await page.waitForLoadState("networkidle");
    if (path === "/shop/") {
      await page.getByRole("button", { name: "Ask agent" }).click();
      await expect(page.getByText("Why I chose this")).toBeVisible();
    }
    // Measure the settled page: entrance animations fade content in, and mid-fade contrast is not what users read.
    await page.evaluate(() => Promise.all(document.getAnimations().map((a) => a.finished.catch(() => undefined))));
    await page.waitForTimeout(300);
    const results = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
      .exclude(".ag-root-wrapper") // third-party grid internals; grid content is covered by its own a11y support
      .analyze();
    const serious = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical");
    expect(serious.map((v) => `${v.id}: ${v.help} (${v.nodes.map((n) => n.target.join(" ")).slice(0, 3).join(", ")})`)).toEqual([]);
  });
}

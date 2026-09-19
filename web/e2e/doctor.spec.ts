import { expect, test, type Page, type Route } from "@playwright/test";

const now = "2026-01-01T00:00:00Z";

async function fulfillJSON(route: Route, value: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
}

async function installDoctorAPI(page: Page) {
  await page.route("**/up", (route) => fulfillJSON(route, { status: "ok" }));
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/status") return fulfillJSON(route, { instance_id: "test", version: "test", health: "healthy", started_at: now, now });
    if (path === "/api/v1/setup") return fulfillJSON(route, { state: "complete", local_ready: true, checks: [] });
    if (path === "/api/v1/events") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/doctor") {
      return fulfillJSON(route, {
        healthy: true,
        generated_at: now,
        checks: [{ name: "disk", status: "healthy", message: "Disk usage is healthy", detail: "38% used" }],
      });
    }
    return fulfillJSON(route, { error: { code: "not_found", message: `No mock for ${path}` } }, 404);
  });
  await page.route("**/api/v1/events/stream*", (route) => route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream" }, body: "" }));
}

test("healthy checks omit corrective advice", async ({ page }) => {
  await installDoctorAPI(page);

  await page.goto("/doctor");

  const diskCheck = page.locator("article").filter({ hasText: "disk" });
  await expect(diskCheck.getByText("38% used", { exact: true })).toBeVisible();
  await expect(diskCheck.getByText("Remediation:", { exact: true })).toHaveCount(0);
});

import { expect, test, type Page, type Route } from "@playwright/test";

const now = "2026-01-01T00:00:00Z";
const unreadEvents = Array.from({ length: 50 }, (_, index) => ({
  id: `event-${index + 1}`,
  type: "service.warning",
  severity: "warning",
  message: `Unread event ${index + 1}`,
  created_at: now,
}));

async function fulfillJSON(route: Route, value: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
}

async function installEventsAPI(page: Page) {
  await page.route("**/up", (route) => fulfillJSON(route, { status: "ok" }));
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/status") return fulfillJSON(route, { instance_id: "test", version: "test", health: "healthy", started_at: now, now });
    if (path === "/api/v1/setup") return fulfillJSON(route, { state: "complete", local_ready: true, checks: [] });
    if (path === "/api/v1/events") return fulfillJSON(route, { items: unreadEvents });
    if (path === "/api/v1/events/unread-count") return fulfillJSON(route, { unread_count: 60 });
    if (path === "/api/v1/ntfy") return fulfillJSON(route, { enabled: false, topic: "" });
    if (path === "/api/v1/applications") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/backups") return fulfillJSON(route, { items: [] });
    return fulfillJSON(route, { error: { code: "not_found", message: `No mock for ${path}` } }, 404);
  });
  await page.route("**/api/v1/events/stream*", async (route) => {
    const duplicate = unreadEvents[0];
    await route.fulfill({
      status: 200,
      headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
      body: `id: ${duplicate.id}\ndata: ${JSON.stringify(duplicate)}\n\n`,
    });
  });
}

test("unread total survives page limits and reload", async ({ page }) => {
  await installEventsAPI(page);

  await page.goto("/events");
  await expect(page.locator(".nav-badge")).toHaveText("60");
  await expect(page.getByText("60 unread total", { exact: true })).toBeVisible();

  await page.reload();
  await expect(page.locator(".nav-badge")).toHaveText("60");
  await expect(page.getByText("60 unread total", { exact: true })).toBeVisible();

  await page.goto("/");
  const unreadMetric = page.locator(".metric-strip article").filter({ hasText: "Unread events" });
  await expect(unreadMetric.getByText("60", { exact: true })).toBeVisible();
});

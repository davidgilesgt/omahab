import { expect, test, type Page, type Route } from "@playwright/test";

const now = "2026-01-01T00:00:00Z";

async function fulfillJSON(route: Route, value: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
}

async function installAdminAPI(page: Page) {
  // Auth requires the panel bearer token (sessionStorage) on every admin
  // route; seed it so the specs exercise pages, not the login gate.
  await page.addInitScript(() => {
    try {
      sessionStorage.setItem("omahab.session", "test-token");
    } catch {}
  });
  await page.route("**/up", (route) => fulfillJSON(route, { status: "ok" }));
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/status") return fulfillJSON(route, { instance_id: "test", version: "test", health: "healthy", started_at: now, now });
    if (path === "/api/v1/setup") return fulfillJSON(route, { state: "complete", local_ready: true, checks: [] });
    if (path === "/api/v1/applications") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/backups") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/events") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/events/unread-count") return fulfillJSON(route, { unread_count: 0 });
    return fulfillJSON(route, { error: { code: "not_found", message: `No mock for ${path}` } }, 404);
  });
  await page.route("**/api/v1/events/stream*", (route) => route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream" }, body: "" }));
}

// Regression for F-admin-index-blank: the exact /admin redirect outranked
// /admin/* at /admin/ and rendered a blank self-redirect (only the toast
// stack, zero body text). Direct /admin/ must render the Overview dashboard.
test("direct /admin/ renders the overview dashboard", async ({ page }) => {
  await installAdminAPI(page);

  await page.goto("/admin/");

  await expect(page).toHaveURL(/\/admin\/$/);
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  await expect(page.getByText("Control plane")).toBeVisible();
  await expect(page.locator("main .page").getByText("Everything is quiet")).toBeVisible();
});

// /admin without the trailing slash is served by the same splat route.
test("/admin without trailing slash renders the overview dashboard", async ({ page }) => {
  await installAdminAPI(page);

  await page.goto("/admin");

  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  await expect(page.getByText("Control plane")).toBeVisible();
});

// The brand link under /admin/ is the link entry back to the index.
test("admin shell brand links to the admin index", async ({ page }) => {
  await installAdminAPI(page);

  await page.goto("/admin/doctor");

  const brand = page.getByRole("link", { name: "Omahab overview" });
  await expect(brand).toHaveAttribute("href", "/admin/");
  await brand.click();
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
});

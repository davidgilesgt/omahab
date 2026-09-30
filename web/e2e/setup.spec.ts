import { expect, test, type Page, type Route } from "@playwright/test";

const now = "2026-01-01T00:00:00Z";

async function fulfillJSON(route: Route, value: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
}

async function installSetupAPI(page: Page) {
  let setupRequests = 0;
  let retryStarted = false;
  await page.addInitScript(() => sessionStorage.setItem("omahab.session", "test-token"));

  await page.route("**/up", (route) => fulfillJSON(route, { status: "ok" }));
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;

    if (path === "/api/v1/status") {
      return fulfillJSON(route, { instance_id: "test", version: "test", health: "healthy", started_at: now, now });
    }
    if (path === "/api/v1/events") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/setup/reconcile" && request.method() === "POST") {
      retryStarted = true;
      return fulfillJSON(route, {});
    }
    if (path === "/api/v1/setup") {
      setupRequests += 1;
      if (retryStarted) {
        return fulfillJSON(route, {
          state: "complete",
          local_ready: true,
          checks: [{ id: "automatic_reconciliation", label: "Automatic reconciliation", owner: "system", status: "ok", detail: "Reconciliation complete" }],
        });
      }
      if (setupRequests > 1) {
        return fulfillJSON(route, {
          state: "attention",
          local_ready: true,
          checks: [{ id: "automatic_reconciliation", label: "Automatic reconciliation", owner: "system", status: "failed", detail: "CI DNS could not be updated", action: "Retry automatic setup" }],
        });
      }
      return fulfillJSON(route, { state: "reconciling", local_ready: true, checks: [] });
    }
    if (path === "/api/v1/users") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/instance") return fulfillJSON(route, { domain: "example.test", tailnet: "test.ts.net", tailscale_ip: "100.64.0.10", assistant_name: "Oma", assistant_slug: "oma", created_at: now });
    if (path === "/api/v1/secrets") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/tailscale/status") return fulfillJSON(route, { running: true, ip: "100.64.0.10", state: "Running" });
    if (path === "/api/v1/network/status") return fulfillJSON(route, { placement: "lan", lan_closed: false, tailscale_running: true });
    if (path === "/api/v1/system/ssh-keys") return fulfillJSON(route, { username: "omahab", items: [] });
    if (path === "/api/v1/setup/models") return fulfillJSON(route, { management_url: "https://ai.example.test", deployments: [], aliases: [] });
    return fulfillJSON(route, { error: { code: "not_found", message: `No mock for ${path}` } }, 404);
  });
  await page.route("**/api/v1/events/stream*", async (route) => {
    await page.waitForTimeout(250);
    const event = { id: "setup-failed", type: "setup.failed", severity: "error", message: "Automatic setup failed", created_at: now };
    await route.fulfill({
      status: 200,
      headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
      body: `id: setup-failed\ndata: ${JSON.stringify(event)}\n\n`,
    });
  });
}

const fullChecks = [
  { id: "restore", label: "Backups: restore or start new", owner: "operator", status: "pending", detail: "restore from an earlier server or start new" },
  { id: "recovery_key", label: "Save a recovery phrase", owner: "operator", status: "pending", detail: "generate and confirm a recovery phrase" },
  { id: "backups_configured", label: "Configure backups", owner: "operator", status: "pending", detail: "no backup repository" },
  { id: "ssh_keys", label: "Add SSH keys", owner: "operator", status: "ok", detail: "ssh keys installed" },
  { id: "tailscale", label: "Connect Tailscale", owner: "operator", status: "ok", detail: "100.64.0.10" },
  { id: "domain", label: "Choose your domain", owner: "operator", status: "ok", detail: "example.test" },
  { id: "cloudflare_dns", label: "Connect Cloudflare DNS", owner: "operator", status: "pending", detail: "secret cloudflare_dns not configured" },
];

async function installStaticSetupAPI(page: Page) {
  await page.addInitScript(() => sessionStorage.setItem("omahab.session", "test-token"));
  await page.route("**/up", (route) => fulfillJSON(route, { status: "ok" }));
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/status") return fulfillJSON(route, { instance_id: "test", version: "test", health: "healthy", started_at: now, now });
    if (path === "/api/v1/events") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/setup") return fulfillJSON(route, { state: "attention", local_ready: true, checks: fullChecks });
    if (path === "/api/v1/users") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/instance") return fulfillJSON(route, { domain: "example.test", tailnet: "test.ts.net", tailscale_ip: "100.64.0.10", assistant_name: "Oma", assistant_slug: "oma", created_at: now });
    if (path === "/api/v1/secrets") return fulfillJSON(route, { items: [] });
    if (path === "/api/v1/tailscale/status") return fulfillJSON(route, { running: true, ip: "100.64.0.10", state: "Running" });
    if (path === "/api/v1/network/status") return fulfillJSON(route, { placement: "lan", lan_closed: false, tailscale_running: true });
    if (path === "/api/v1/system/ssh-keys") return fulfillJSON(route, { username: "omahab", items: [] });
    if (path === "/api/v1/setup/models") return fulfillJSON(route, { management_url: "https://ai.example.test", deployments: [], aliases: [] });
    if (path === "/api/v1/recovery/instances") return fulfillJSON(route, { instances: [] });
    return fulfillJSON(route, { error: { code: "not_found", message: `No mock for ${path}` } }, 404);
  });
}

test("backups is the first setup step and asks about restoring", async ({ page }) => {
  await installStaticSetupAPI(page);

  await page.goto("/setup");

  const titles = page.locator(".setup-box-title");
  await expect(titles.first()).toHaveText("Backups");
  await expect(titles.nth(1)).toHaveText("SSH keys");
  await expect(titles.nth(2)).toHaveText("Tailscale");
  await expect(page.getByText("Do you have a recovery phrase from an earlier Omahab server?")).toBeVisible();
  await expect(page.getByRole("button", { name: "Find earlier servers" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "No — start fresh" })).toBeVisible();
});

test("automatic failure is visible without reload", async ({ page }) => {
  await installSetupAPI(page);

  await page.goto("/setup");

  await expect(page.getByText("CI DNS could not be updated")).toBeVisible();
  await page.getByRole("button", { name: "Retry automatic setup", exact: true }).first().click();
  await expect(page.getByText("Reconciliation complete")).toBeVisible();
  await page.unrouteAll({ behavior: "ignoreErrors" });
});

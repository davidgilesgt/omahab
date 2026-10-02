import { expect, test, type Page, type Route } from "@playwright/test";

const now = "2026-01-01T00:00:00Z";

async function fulfillJSON(route: Route, value: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
}

async function installDoctorAPI(page: Page) {
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

// Regression for F-doctor-copy-no-feedback: the button used the raw async
// clipboard with no fallback, so insecure contexts and denied permissions
// produced neither a copy nor any toast. Every path must surface feedback.
test("copy as markdown shows a success toast and writes the clipboard", async ({ page, context }) => {
  await installDoctorAPI(page);
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);

  await page.goto("/doctor");
  await page.getByRole("button", { name: "Copy as Markdown", exact: true }).click();

  await expect(page.locator(".toast-success").getByText("Doctor report copied as Markdown")).toBeVisible();
  const pasted = await page.evaluate(() => navigator.clipboard.readText());
  expect(pasted).toContain("# Doctor report");
  expect(pasted).toContain("| disk | healthy |");
});

test("copy as markdown shows an error toast when the clipboard is blocked", async ({ page }) => {
  await installDoctorAPI(page);
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: () => Promise.reject(new Error("denied")), readText: () => Promise.reject(new Error("denied")) },
      configurable: true,
    });
    document.execCommand = () => false;
  });

  await page.goto("/doctor");
  await page.getByRole("button", { name: "Copy as Markdown", exact: true }).click();

  await expect(page.locator(".toast-error").getByText("Could not copy to clipboard")).toBeVisible();
});

test("copy as markdown explains insecure HTTP when no clipboard API exists", async ({ page }) => {
  await installDoctorAPI(page);
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
    document.execCommand = () => false;
  });

  await page.goto("/doctor");
  await page.getByRole("button", { name: "Copy as Markdown", exact: true }).click();

  await expect(page.locator(".toast-error").getByText("Clipboard is unavailable over insecure HTTP")).toBeVisible();
});

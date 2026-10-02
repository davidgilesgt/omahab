#!/usr/bin/env node
// Real installed-system journey. No routes, mocks, traces, or secret captures.
import { chromium, expect } from "@playwright/test";
import { readFileSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

const [baseURL, evidenceDir, mode = "standard"] = process.argv.slice(2);
const token = readFileSync(0, "utf8").trim();
if (!baseURL || !evidenceDir || !/^[a-z0-9]{8}$/.test(token)) {
  throw new Error("Installed browser journey requires URL, evidence directory, and token on stdin");
}
mkdirSync(evidenceDir, { recursive: true });
const checks = [];
let stage = "launch";
let browser;
let failed = false;
let appliedOperation;
let caddyProcesses;
async function check(name, action) {
  stage = name;
  await action();
  checks.push({ name, status: "PASS" });
}
try {
  browser = await chromium.launch({
    executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined,
  });
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await context.newPage();
  const auth = { Authorization: `Bearer ${token}` };
  // Visibility ignores opacity. Finish the panel's finite entrance fades
  // before saving evidence; perpetual pulse/spin animations do not block it.
  async function capture(path) {
    await page.waitForFunction(() => document.getAnimations().every((animation) => {
      const timing = animation.effect?.getComputedTiming?.();
      return animation.playState === "finished" || timing?.iterations === Infinity;
    }), null, { timeout: 10_000 });
    await page.screenshot({ path, fullPage: true, animations: "disabled" });
  }
  async function setup() {
    const response = await context.request.get(`${baseURL}/api/v1/setup`, { headers: auth });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function setupState(id) {
    return (await setup()).checks.find((item) => item.id === id)?.status;
  }
  await check("anonymous browser redirects to login", async () => {
    await page.goto(`${baseURL}/setup`);
    await expect(page.getByLabel("Access token")).toBeVisible();
    expect(new URL(page.url()).pathname).toBe("/login");
  });
  await check("invalid token cannot sign in", async () => {
    await page.getByLabel("Access token").fill("invalid-token");
    await page.getByRole("button", { name: "Continue", exact: true }).click();
    await expect(page.getByRole("alert")).toBeVisible();
    expect(new URL(page.url()).pathname).toBe("/login");
  });
  await check("installed token signs in to setup", async () => {
    await page.getByLabel("Access token").fill(token);
    await page.getByRole("button", { name: "Continue", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Setup & Connect Services", exact: true })).toBeVisible();
    expect(new URL(page.url()).pathname).toBe("/setup");
  });
  await capture(join(evidenceDir, mode === "apply" ? "bundle-before.png" : "setup-desktop.png"));
  if (mode === "standard") {
    await check("start-new choice persists after browser reload", async () => {
      await page.getByRole("button", { name: "Start new instead", exact: true }).click();
      await expect.poll(() => setupState("restore")).toBe("skipped");
      await page.reload();
      await expect(page.getByRole("button", { name: "Generate recovery phrase", exact: true })).toBeVisible();
      expect(await setupState("restore")).toBe("skipped");
    });
    await check("recovery acknowledgement and check code gate confirmation", async () => {
      await page.getByRole("button", { name: "Generate recovery phrase", exact: true }).click();
      const acknowledgement = page.getByRole("checkbox", { name: "I saved this in my password manager" });
      await expect(acknowledgement).toBeVisible();
      await expect(page.getByRole("button", { name: "Confirm and store kit", exact: true })).toHaveCount(0);
      await acknowledgement.check();
      const code = page.getByLabel("Enter the check code", { exact: true });
      const confirm = page.getByRole("button", { name: "Confirm and store kit", exact: true });
      const fingerprint = await code.getAttribute("placeholder");
      expect(fingerprint).toMatch(/^[a-f0-9]+$/i);
      await code.fill("incorrect");
      await expect(confirm).toBeDisabled();
      const rejected = await context.request.post(`${baseURL}/api/v1/recovery/confirm`, {
        headers: auth, data: { fingerprint: "incorrect", saved_ack: true },
      });
      expect(rejected.status()).toBe(404);
      expect(await setupState("recovery_key")).not.toBe("ok");
      const unacknowledged = await context.request.post(`${baseURL}/api/v1/recovery/confirm`, {
        headers: auth, data: { fingerprint, saved_ack: false },
      });
      expect(unacknowledged.status()).toBe(400);
      expect(await setupState("recovery_key")).not.toBe("ok");
      await code.fill(fingerprint);
      await expect(confirm).toBeEnabled();
      await confirm.click();
      await expect.poll(() => setupState("recovery_key")).toBe("ok");
      await expect(page.locator(".recovery-grid")).toHaveCount(0);
    });
    await check("confirmed recovery survives browser reload", async () => {
      await page.reload();
      await expect(page.getByRole("button", { name: "Generate recovery phrase", exact: true })).toBeVisible();
      expect(await setupState("recovery_key")).toBe("ok");
      await expect(page.locator(".recovery-grid")).toHaveCount(0);
    });
    await check("Caddy install and browser restart reach healthy running service", async () => {
      async function caddyPID() {
        if (!process.env.E2E_SSH_KEY || !process.env.E2E_SSH_PORT) {
          throw new Error("Installed lifecycle checks require the harness SSH connection");
        }
        const { stdout } = await execFileAsync("ssh", [
          "-T", "-i", process.env.E2E_SSH_KEY, "-p", process.env.E2E_SSH_PORT,
          "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
          "admin@127.0.0.1", "systemctl show caddy.service -p MainPID --value",
        ], { timeout: 15_000 });
        const pid = Number(stdout.trim());
        expect(pid).toBeGreaterThan(0);
        return pid;
      }
      const install = await context.request.post(`${baseURL}/api/v1/applications`, {
        headers: auth, data: { bundle_id: "caddy", name: "e2e-caddy", exposure: "private" },
      });
      expect([201, 409]).toContain(install.status());
      const listed = await context.request.get(`${baseURL}/api/v1/applications`, { headers: auth });
      expect(listed.status()).toBe(200);
      const application = (await listed.json()).items.find((item) => item.name === "e2e-caddy");
      if (!application) throw new Error("Installed Caddy fixture is absent");
      await page.goto(`${baseURL}/applications`);
      const row = page.getByRole("row").filter({ has: page.getByText("e2e-caddy", { exact: true }) });
      await expect(row).toBeVisible();
      const before = await caddyPID();
      const actionResponse = page.waitForResponse((response) =>
        response.url().endsWith(`/applications/${application.id}/actions`) && response.request().method() === "POST");
      await row.getByRole("button", { name: "restart", exact: true }).click();
      expect((await actionResponse).status()).toBe(200);
      const after = await caddyPID();
      expect(after).not.toBe(before);
      caddyProcesses = { before_pid: before, after_pid: after };
      await expect.poll(async () => {
        const response = await context.request.post(`${baseURL}/api/v1/applications/${application.id}/actions`, {
          headers: auth, data: { action: "check_health" },
        });
        expect(response.status()).toBe(200);
        return (await response.json()).health;
      }, { timeout: 60_000 }).toBe("healthy");
      await page.reload();
      await expect(row.getByText(/^healthy$/i)).toBeVisible();
      await capture(join(evidenceDir, "applications.png"));
      await page.goto(`${baseURL}/setup`);
    });
    await check("device credentials cannot administer the server and revocation denies access", async () => {
      const before = await context.request.get(`${baseURL}/api/v1/companion/devices`, { headers: auth });
      expect(before.status()).toBe(200);
      const previous = new Set((await before.json()).items.map((device) => device.id));
      const enrollment = await context.request.post(`${baseURL}/api/v1/companion-enrollments`, { headers: auth });
      expect(enrollment.status()).toBe(201);
      const { code } = await enrollment.json();
      const claim = await context.request.post(`${baseURL}/api/v1/companion/enroll`, { data: { code } });
      expect(claim.status()).toBe(201);
      const { token: deviceToken } = await claim.json();
      const deviceAuth = { Authorization: `Bearer ${deviceToken}` };
      let deviceId;
      try {
        const listed = await context.request.get(`${baseURL}/api/v1/companion/devices`, { headers: auth });
        expect(listed.status()).toBe(200);
        deviceId = (await listed.json()).items.find((device) => !previous.has(device.id))?.id;
        if (!deviceId) throw new Error("Newly enrolled device is absent");
        const permitted = await context.request.get(`${baseURL}/api/v1/companion/status`, { headers: deviceAuth });
        expect(permitted.status()).toBe(200);
        const deniedAdmin = await context.request.get(`${baseURL}/api/v1/setup`, { headers: deviceAuth });
        expect(deniedAdmin.status()).toBe(403);
        const deniedDevice = await context.request.get(`${baseURL}/api/v1/companion/status`, { headers: auth });
        expect(deniedDevice.status()).toBe(403);
        const deniedMutation = await context.request.post(`${baseURL}/api/v1/recovery/generate`, { headers: deviceAuth });
        expect(deniedMutation.status()).toBe(403);
      } finally {
        if (deviceId) {
          const revoked = await context.request.delete(`${baseURL}/api/v1/companion/devices/${deviceId}`, { headers: auth });
          expect(revoked.status()).toBe(204);
        }
      }
      const revokedAccess = await context.request.get(`${baseURL}/api/v1/companion/status`, { headers: deviceAuth });
      expect(revokedAccess.status()).toBe(403);
    });
  }
  if (mode === "apply") {
    await check("bundle browser action starts a real installed apply operation", async () => {
      await page.goto(`${baseURL}/bundles`);
      const responsePromise = page.waitForResponse((response) =>
        new URL(response.url()).pathname === "/api/v1/bundles/apply" && response.request().method() === "POST");
      await page.getByRole("button", { name: "Apply bundles", exact: true }).click();
      const response = await responsePromise;
      expect(response.status()).toBe(202);
      appliedOperation = await response.json();
      await expect(page.getByRole("status").filter({ hasText: appliedOperation.id })).toBeVisible();
      await capture(join(evidenceDir, "bundle-apply.png"));
    });
  } else {
    await check("setup is usable on a narrow screen", async () => {
      await page.setViewportSize({ width: 390, height: 844 });
      await expect(page.getByRole("heading", { name: "Setup & Connect Services", exact: true })).toBeVisible();
      const overflows = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1);
      expect(overflows).toBe(false);
      await capture(join(evidenceDir, "setup-mobile.png"));
    });
  }
} catch {
  failed = true;
  checks.push({ name: stage, status: "FAIL" });
  // Playwright errors can include secret-bearing DOM. Do not print them.
  console.error(`Installed browser journey failed at: ${stage}`);
} finally {
  writeFileSync(join(evidenceDir, mode === "apply" ? "bundle-browser.json" : "browser.json"), JSON.stringify({
    status: failed ? "FAIL" : "PASS", browser_version: browser?.version(),
    mode, checks, simulated: false, caddy_processes: caddyProcesses,
    applied_operation: appliedOperation && {
      id: appliedOperation.id, status: appliedOperation.status, stage: appliedOperation.stage,
    },
  }, null, 2) + "\n");
  await browser?.close();
}
if (failed) process.exitCode = 1;
else if (appliedOperation) console.log(JSON.stringify({ id: appliedOperation.id }));

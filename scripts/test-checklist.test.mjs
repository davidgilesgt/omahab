import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync, rmSync, statSync, symlinkSync, utimesSync } from "node:fs";
import { join, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "..");
const SCRIPT = join(ROOT, "scripts", "test-checklist.mjs");
const mod = await import("./test-checklist.mjs");

function run(args, opts = {}) {
  try {
    const out = execFileSync("node", [SCRIPT, ...args], { cwd: ROOT, encoding: "utf8", ...opts });
    return { ok: true, out, err: "" };
  } catch (e) {
    return { ok: false, out: e.stdout || "", err: e.stderr || String(e.message) };
  }
}

const isoRoot = join(ROOT, "dist", "deep-test", `.test-${process.pid}`);
before(() => {
  mkdirSync(isoRoot, { recursive: true });
});
after(() => {
  rmSync(isoRoot, { recursive: true, force: true });
});

function tmpRunName(tag) {
  return `dist/deep-test/.test-${process.pid}-${tag}`;
}

describe("generator isolation and safety", () => {
  it("generating into an existing run fails without changing evidence", () => {
    const rel = tmpRunName("exists");
    const abs = join(ROOT, rel);
    rmSync(abs, { recursive: true, force: true });
    const first = run(["new", "--output", rel]);
    assert.equal(first.ok, true, first.err);
    const jsonPath = join(abs, "checklist.json");
    const beforeJson = readFileSync(jsonPath, "utf8");
    const evPath = join(abs, "evidence", "probe.txt");
    writeFileSync(evPath, "probe");
    const beforeStat = statSync(jsonPath).mtimeMs;
    const second = run(["new", "--output", rel]);
    assert.equal(second.ok, false);
    assert.match(second.err, /already exists/);
    assert.equal(readFileSync(jsonPath, "utf8"), beforeJson);
    assert.equal(readFileSync(evPath, "utf8"), "probe");
    assert.equal(statSync(jsonPath).mtimeMs, beforeStat);
    rmSync(abs, { recursive: true, force: true });
  });

  it("second run does not copy completed statuses", () => {
    const rel1 = tmpRunName("blank1");
    const rel2 = tmpRunName("blank2");
    for (const r of [rel1, rel2]) rmSync(join(ROOT, r), { recursive: true, force: true });
    assert.equal(run(["new", "--output", rel1]).ok, true);
    const j1 = join(ROOT, rel1, "checklist.json");
    const obj = JSON.parse(readFileSync(j1, "utf8"));
    obj.cases[0].status = "PASS";
    obj.cases[0].actual = "done";
    obj.cases[0].owner = "tester";
    const evRel = "evidence/done.txt";
    writeFileSync(join(ROOT, rel1, evRel), "x");
    obj.cases[0].evidence = [{ type: obj.cases[0].evidence_types.includes("BROWSER") ? "BROWSER" : obj.cases[0].evidence_types[0], path: evRel }];
    writeFileSync(j1, JSON.stringify(obj, null, 2));
    assert.equal(run(["new", "--output", rel2]).ok, true);
    const obj2 = JSON.parse(readFileSync(join(ROOT, rel2, "checklist.json"), "utf8"));
    assert.ok(obj2.cases.every((c) => c.status === "PENDING" && c.actual === "" && c.evidence.length === 0));
    rmSync(join(ROOT, rel1), { recursive: true, force: true });
    rmSync(join(ROOT, rel2), { recursive: true, force: true });
  });

  it("output traversal and symlink escape are rejected", () => {
    const trav = run(["new", "--output", "dist/../outside-evil"]);
    assert.equal(trav.ok, false);
    assert.match(trav.err, /beneath dist/);
    assert.equal(existsSync(join(ROOT, "outside-evil")), false);
    const link = join(ROOT, "dist", `.test-link-${process.pid}`);
    const target = join(isoRoot, "link-target");
    mkdirSync(target, { recursive: true });
    try { rmSync(link, { force: true }); } catch {}
    symlinkSync(target, link);
    // Resolve a path through the symlink that escapes dist/ realpath.
    const evil = run(["new", "--output", `dist/.test-link-${process.pid}/../../outside2`]);
    assert.equal(evil.ok, false);
    rmSync(link, { force: true });
  });

  it("generator output never includes credential sentinels", () => {
    const sentinel = `SENTINEL-${process.pid}-CRED-SECRET`;
    const decoy = join(isoRoot, "decoy-credentials.json");
    writeFileSync(decoy, JSON.stringify({ domain: `${sentinel}-domain`, cloudflare_a: sentinel }));
    const rel = tmpRunName("sentinel");
    const abs = join(ROOT, rel);
    rmSync(abs, { recursive: true, force: true });
    const r = run(["new", "--output", rel]);
    assert.equal(r.ok, true, r.err);
    for (const f of ["checklist.json", "checklist.md", "context.md"]) {
      const txt = readFileSync(join(abs, f), "utf8");
      assert.ok(!txt.includes(sentinel));
    }
    const scriptSrc = readFileSync(SCRIPT, "utf8");
    assert.ok(!scriptSrc.includes("credentials.json"));
    rmSync(abs, { recursive: true, force: true });
  });
});

describe("coverage validation without touching product files", () => {
  const goodSources = ["web/src/App.tsx", "internal/api/server.go"];
  function baseCase(id, covers) {
    return { id, area: "t", title: `t ${id}`, covers, sources: goodSources, requires: [], steps: ["s"], expected: ["e"], evidence_types: ["SOURCE_REVIEW"] };
  }
  it("new catalog app without cases fails with its identifier", () => {
    const obj = { version: 1, cases: [baseCase("a.one", ["app:alpha"])] };
    const v = mod.validateTemplateObject(obj, { root: ROOT, catalogIds: ["alpha", "beta"], uiRoutes: [], apiRoutes: [] });
    assert.ok(v.errors.some((e) => e.includes("app:beta")));
  });
  it("new UI route without cases fails with exact reference", () => {
    const obj = { version: 1, cases: [baseCase("u.one", ["ui:/known"])] };
    const v = mod.validateTemplateObject(obj, { root: ROOT, catalogIds: [], uiRoutes: ["/known", "/fresh"], apiRoutes: [] });
    assert.ok(v.errors.some((e) => e === "missing UI coverage: ui:/fresh"));
  });
  it("new API registration without cases fails with exact reference", () => {
    const obj = { version: 1, cases: [baseCase("p.one", ["api:GET /known"])] };
    const v = mod.validateTemplateObject(obj, { root: ROOT, catalogIds: [], uiRoutes: [], apiRoutes: ["GET /known", "POST /fresh"] });
    assert.ok(v.errors.some((e) => e === "missing API coverage: api:POST /fresh"));
  });
  it("removed entries with retained refs fail as stale", () => {
    const obj = { version: 1, cases: [baseCase("s.one", ["app:ghost", "ui:/ghost", "api:GET /ghost"])] };
    const v = mod.validateTemplateObject(obj, { root: ROOT, catalogIds: [], uiRoutes: [], apiRoutes: [] });
    assert.ok(v.errors.some((e) => e.includes("stale application reference: app:ghost")));
    assert.ok(v.errors.some((e) => e.includes("stale UI reference: ui:/ghost")));
    assert.ok(v.errors.some((e) => e.includes("stale API reference: api:GET /ghost")));
  });
  it("malformed, duplicate, and missing instruction files fail", () => {
    const dup = { version: 1, cases: [baseCase("dup.x", ["manual:feat"]), baseCase("dup.x", ["manual:feat"])] };
    const vdup = mod.validateTemplateObject(dup, { root: ROOT, catalogIds: [], uiRoutes: [], apiRoutes: [] });
    assert.ok(vdup.errors.some((e) => e.includes("duplicate identifier: dup.x")));
    const bad = { version: 1, cases: [{ id: "BAD", area: "", title: "", covers: [], sources: [], requires: [], steps: [], expected: [], evidence_types: [] }] };
    const vbad = mod.validateTemplateObject(bad, { root: ROOT, catalogIds: [], uiRoutes: [], apiRoutes: [] });
    assert.ok(vbad.errors.length > 0);
    const emptyRoot = mkdtempSync(join(isoRoot, "empty-"));
    const vmiss = mod.validateTemplateObject({ version: 1, cases: [baseCase("m.one", ["manual:feat"])] }, { root: emptyRoot, catalogIds: [], uiRoutes: [], apiRoutes: [] });
    assert.ok(vmiss.errors.some((e) => e.includes("missing instruction file")));
    assert.ok(vmiss.errors.some((e) => e.includes("missing source path")));
  });
  it("nonliteral route and dynamic API forms are rejected", () => {
    assert.throws(() => mod.extractUiRoutesFromText(`<Route path={dyn} element={<X/>} />`), /nonliteral/);
    const badApi = `func (s *Server) buildRouter() chi.Router {\n r := chi.NewRouter()\n r.Get(dynamicPath, h)\n return r\n}`;
    assert.throws(() => mod.extractApiRoutesFromText(badApi), /dynamic API path/);
  });
});

describe("run validation and rendering", () => {
  it("check --run and render --run agree with saved states", () => {
    const rel = tmpRunName("runcheck");
    const abs = join(ROOT, rel);
    rmSync(abs, { recursive: true, force: true });
    assert.equal(run(["new", "--output", rel]).ok, true);
    const jpath = join(abs, "checklist.json");
    const obj = JSON.parse(readFileSync(jpath, "utf8"));
    // Make first case FAIL, second BLOCKED.
    const c0 = obj.cases[0];
    c0.status = "FAIL";
    c0.actual = "reproduced failure";
    c0.owner = "tester";
    const ev0 = "evidence/fail.txt";
    writeFileSync(join(abs, ev0), "fail evidence");
    c0.evidence = [{ type: c0.evidence_types[0], path: ev0 }];
    const c1 = obj.cases[1];
    c1.status = "BLOCKED";
    c1.reason = "missing test domain";
    writeFileSync(jpath, JSON.stringify(obj, null, 2));
    const chk = run(["check", "--run", rel]);
    assert.equal(chk.ok, true, chk.err);
    assert.match(chk.out, /FAIL 1/);
    assert.match(chk.out, /BLOCKED 1/);
    const ren = run(["render", "--run", rel]);
    assert.equal(ren.ok, true, ren.err);
    const md = readFileSync(join(abs, "checklist.md"), "utf8");
    assert.ok(md.includes(c0.id) && md.includes("FAIL"));
    assert.ok(md.includes(c1.id) && md.includes("BLOCKED"));
    // Failed/blocked must not be marked passed.
    const failLine = md.split("\n").find((l) => l.includes(c0.id));
    const blockLine = md.split("\n").find((l) => l.includes(c1.id));
    assert.ok(failLine.startsWith("- [ ]"));
    assert.ok(blockLine.startsWith("- [ ]"));
    assert.ok(!md.includes(`\`${c0.id}\` —`) || failLine.includes("FAIL"));
    rmSync(abs, { recursive: true, force: true });
  });

  it("PASS without required BROWSER evidence is rejected", () => {
    const rel = tmpRunName("browserreq");
    const abs = join(ROOT, rel);
    rmSync(abs, { recursive: true, force: true });
    assert.equal(run(["new", "--output", rel]).ok, true);
    const jpath = join(abs, "checklist.json");
    const obj = JSON.parse(readFileSync(jpath, "utf8"));
    const target = obj.cases.find((c) => c.evidence_types.includes("BROWSER"));
    assert.ok(target);
    target.status = "PASS";
    target.actual = "ok";
    const evRel = "evidence/api-only.txt";
    writeFileSync(join(abs, evRel), "api");
    target.evidence = [{ type: "LIVE_API", path: evRel }];
    writeFileSync(jpath, JSON.stringify(obj, null, 2));
    const chk = run(["check", "--run", rel]);
    assert.equal(chk.ok, false);
    assert.match(chk.err, /requires BROWSER evidence/);
    rmSync(abs, { recursive: true, force: true });
  });
});

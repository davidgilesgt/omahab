#!/usr/bin/env node
/**
 * Maintained deep-test checklist tool.
 *
 * Commands (repository root is resolved from this script, not CWD):
 *   node scripts/test-checklist.mjs context
 *   node scripts/test-checklist.mjs new [--output <rel>]
 *   node scripts/test-checklist.mjs check [--run <dir>]
 *   node scripts/test-checklist.mjs render --run <dir>
 *
 * Node standard modules only. Standalone style like
 * workers/email/scripts/generate-vars.mjs.
 */

import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  realpathSync,
  renameSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { createHash } from "node:crypto";
import { dirname, join, normalize, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { execSync } from "node:child_process";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(SCRIPT_DIR, "..");

const TEMPLATE_REL = "testing/checklist.json";
const CONTEXT_REL = "testing/CONTEXT.md";
const SKILL_BROWSER_REL = "testing/skills/browser-testing.md";
const SKILL_APPLIANCE_REL = "testing/skills/appliance-testing.md";
const AGENTS_REL = "AGENTS.md";
const BUNDLES_GLOB_REL = "bundles/*/bundle.json";
const APP_TSX_REL = "web/src/App.tsx";
const BUNDLES_HASH_INPUT = "nix/module.nix";
const SERVER_GO_REL = "internal/api/server.go";
const VERSION = 1;
const ID_RE = /^[a-z0-9]+(?:[.-][a-z0-9]+)*$/;
const EVIDENCE_TYPES = ["BROWSER", "LIVE_API", "LIVE_CLI", "INTEGRATION", "UNIT", "SOURCE_REVIEW"];
const STATUSES = ["PENDING", "PASS", "FAIL", "BLOCKED", "NOT_APPLICABLE"];
const API_METHODS = ["GET", "POST", "PUT", "PATCH", "DELETE", "ANY", "HEAD", "OPTIONS", "TRACE", "CONNECT"];
const GO_METHODS = ["Get", "Post", "Put", "Patch", "Delete", "Handle", "Head", "Options", "Trace", "Connect"];
const GO_IGNORABLE = ["Use", "Group", "NotFound", "MethodNotAllowed"];

function err(msg) {
  process.stderr.write(`ERROR: ${msg}\n`);
}

function fail(msg, code = 1) {
  err(msg);
  process.exit(code);
}

function sha256Bytes(buf) {
  return createHash("sha256").update(buf).digest("hex");
}

function sha256File(abs) {
  return sha256Bytes(readFileSync(abs));
}

function repoAbs(rel) {
  return join(REPO_ROOT, rel);
}

function readText(rel) {
  const abs = repoAbs(rel);
  if (!existsSync(abs)) fail(`missing file: ${rel}`);
  return readFileSync(abs, "utf8");
}

// --- extraction (pure helpers accept content for tests) ---

export function extractUiRoutesFromText(content) {
  const tagRe = /<Route\b[^>]*>/g;
  const routes = [];
  let m;
  while ((m = tagRe.exec(content)) !== null) {
    const tag = m[0];
    const litD = /path\s*=\s*"([^"]+)"/.exec(tag);
    const litS = /path\s*=\s*'([^']+)'/.exec(tag);
    const nonlit = /path\s*=\s*\{/.test(tag);
    const isIndex = /\bindex\b/.test(tag);
    if (litD) {
      routes.push(litD[1]);
    } else if (litS) {
      routes.push(litS[1]);
    } else if (nonlit) {
      throw new Error(`unsupported nonliteral Route path declaration in ${APP_TSX_REL}`);
    } else if (isIndex) {
      continue;
    } else {
      throw new Error(`Route without literal path in ${APP_TSX_REL} (use literal path or index)`);
    }
  }
  return [...new Set(routes)];
}

export function extractUiRoutes(root = REPO_ROOT) {
  const abs = join(root, APP_TSX_REL);
  if (!existsSync(abs)) throw new Error(`missing file: ${APP_TSX_REL}`);
  return extractUiRoutesFromText(readFileSync(abs, "utf8"));
}

function buildRouterBody(content) {
  const marker = "func (s *Server) buildRouter()";
  const idx = content.indexOf(marker);
  if (idx < 0) throw new Error(`buildRouter not found in ${SERVER_GO_REL}`);
  const open = content.indexOf("{", idx);
  if (open < 0) throw new Error(`buildRouter body not found in ${SERVER_GO_REL}`);
  let depth = 0;
  for (let i = open; i < content.length; i++) {
    if (content[i] === "{") depth++;
    else if (content[i] === "}") {
      depth--;
      if (depth === 0) return content.slice(open, i + 1);
    }
  }
  throw new Error(`unterminated buildRouter in ${SERVER_GO_REL}`);
}

export function extractApiRoutesFromText(content) {
  const body = buildRouterBody(content);
  const callRe = /\br\s*\.\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(/g;
  const out = [];
  let m;
  while ((m = callRe.exec(body)) !== null) {
    const name = m[1];
    if (GO_IGNORABLE.includes(name)) continue;
    if (!GO_METHODS.includes(name)) {
      throw new Error(`unrecognized router registration r.${name} in ${SERVER_GO_REL}`);
    }
    let pos = m.index + m[0].length;
    while (pos < body.length && /\s/.test(body[pos])) pos++;
    if (body[pos] !== '"') {
      throw new Error(`dynamic API path for r.${name} in ${SERVER_GO_REL}`);
    }
    let j = pos + 1;
    let path = "";
    let closed = false;
    while (j < body.length) {
      const c = body[j];
      if (c === "\\") {
        if (j + 1 < body.length) {
          path += body[j + 1];
          j += 2;
          continue;
        }
        break;
      }
      if (c === '"') {
        closed = true;
        break;
      }
      path += c;
      j++;
    }
    if (!closed) throw new Error(`dynamic API path for r.${name} in ${SERVER_GO_REL}`);
    const method = name === "Handle" ? "ANY" : name.toUpperCase();
    out.push(`${method} ${path}`);
  }
  // Detect nested prefixes / mounts explicitly (also caught above, but keep message stable).
  if (/\br\s*\.\s*(Route|Mount)\s*\(/.test(body)) {
    throw new Error(`nested route prefix or mount in ${SERVER_GO_REL}`);
  }
  return [...new Set(out)];
}

export function extractApiRoutes(root = REPO_ROOT) {
  const abs = join(root, SERVER_GO_REL);
  if (!existsSync(abs)) throw new Error(`missing file: ${SERVER_GO_REL}`);
  return extractApiRoutesFromText(readFileSync(abs, "utf8"));
}

export function getBundleIdsFromDocs(docs) {
  if (!Array.isArray(docs)) throw new Error("invalid bundles glob: expected array of documents");
  return docs.map((doc) => {
    if (!doc || typeof doc !== "object" || typeof doc.id !== "string" || doc.id.length === 0) {
      throw new Error("invalid bundle.json: missing id");
    }
    return doc.id;
  });
}

// Curated bundle ids come from the per-bundle files bundles/<id>/bundle.json
// (one catalog Bundle object per file; ids match their directory names).
// The deployed catalog.json is assembled from these at build time, so the
// glob is the one source of truth in the repo.
export function getCatalogIds(root = REPO_ROOT) {
  const dir = join(root, "bundles");
  if (!existsSync(dir)) throw new Error(`missing directory: bundles/`);
  const docs = readdirSync(dir)
    .filter((entry) => existsSync(join(dir, entry, "bundle.json")))
    .sort()
    .map((entry) => JSON.parse(readFileSync(join(dir, entry, "bundle.json"), "utf8")));
  if (docs.length === 0) throw new Error(`no ${BUNDLES_GLOB_REL} found`);
  return getBundleIdsFromDocs(docs);
}

// --- validation ---

export function validateCoverFormat(cover) {
  if (typeof cover !== "string" || cover.length === 0) return "empty cover";
  if (cover.startsWith("app:")) {
    const id = cover.slice(4);
    if (!ID_RE.test(id)) return `invalid app cover: ${cover}`;
    return null;
  }
  if (cover.startsWith("ui:")) {
    const p = cover.slice(3);
    if (p.length === 0) return `invalid ui cover: ${cover}`;
    return null;
  }
  if (cover.startsWith("api:")) {
    const rest = cover.slice(4);
    const sp = rest.indexOf(" ");
    if (sp < 0) return `invalid api cover: ${cover}`;
    const method = rest.slice(0, sp);
    const path = rest.slice(sp + 1);
    if (!API_METHODS.includes(method)) return `invalid api cover: ${cover}`;
    if (!path.startsWith("/")) return `invalid api cover: ${cover}`;
    return null;
  }
  if (cover.startsWith("manual:")) {
    const feat = cover.slice(7);
    if (!ID_RE.test(feat)) return `invalid manual cover: ${cover}`;
    return null;
  }
  return `unsupported cover prefix: ${cover}`;
}

export function validateCaseFields(c) {
  const problems = [];
  if (!c || typeof c !== "object") return ["case is not an object"];
  if (typeof c.id !== "string" || !ID_RE.test(c.id)) problems.push(`invalid id: ${String(c.id)}`);
  if (typeof c.area !== "string" || c.area.length === 0) problems.push(`invalid area for ${c.id}`);
  if (typeof c.title !== "string" || c.title.length === 0) problems.push(`invalid title for ${c.id}`);
  if (!Array.isArray(c.covers) || c.covers.length === 0) problems.push(`missing covers for ${c.id}`);
  else for (const cov of c.covers) {
    const e = validateCoverFormat(cov);
    if (e) problems.push(`${e} (case ${c.id})`);
  }
  if (!Array.isArray(c.sources) || c.sources.length === 0) problems.push(`missing sources for ${c.id}`);
  else for (const s of c.sources) {
    if (typeof s !== "string" || s.length === 0) problems.push(`invalid source for ${c.id}`);
  }
  if (!Array.isArray(c.requires)) problems.push(`missing requires for ${c.id}`);
  else for (const r of c.requires) {
    if (typeof r !== "string" || r.length === 0) problems.push(`invalid require for ${c.id}`);
  }
  if (!Array.isArray(c.steps) || c.steps.length === 0) problems.push(`missing steps for ${c.id}`);
  else for (const s of c.steps) {
    if (typeof s !== "string" || s.length === 0) problems.push(`invalid step for ${c.id}`);
  }
  if (!Array.isArray(c.expected) || c.expected.length === 0) problems.push(`missing expected for ${c.id}`);
  else for (const s of c.expected) {
    if (typeof s !== "string" || s.length === 0) problems.push(`invalid expected for ${c.id}`);
  }
  if (!Array.isArray(c.evidence_types) || c.evidence_types.length === 0) problems.push(`missing evidence_types for ${c.id}`);
  else for (const e of c.evidence_types) {
    if (!EVIDENCE_TYPES.includes(e)) problems.push(`invalid evidence type ${String(e)} for ${c.id}`);
  }
  return problems;
}

export function checkSourcesExist(cases, root = REPO_ROOT) {
  const missing = [];
  for (const c of cases) {
    for (const s of c.sources || []) {
      const abs = join(root, s);
      if (!existsSync(abs)) missing.push(`${c.id}: missing source path ${s}`);
    }
  }
  return missing;
}

export function checkCoverage(cases, { catalogIds, uiRoutes, apiRoutes }) {
  const appRefs = new Set();
  const uiRefs = new Set();
  const apiRefs = new Set();
  for (const c of cases) {
    for (const cov of c.covers || []) {
      if (cov.startsWith("app:")) appRefs.add(cov.slice(4));
      else if (cov.startsWith("ui:")) uiRefs.add(cov.slice(3));
      else if (cov.startsWith("api:")) apiRefs.add(cov.slice(4));
    }
  }
  const catalogSet = new Set(catalogIds);
  const uiSet = new Set(uiRoutes);
  const apiSet = new Set(apiRoutes);
  const missingApps = catalogIds.filter((id) => !appRefs.has(id));
  const staleApps = [...appRefs].filter((id) => !catalogSet.has(id));
  const missingUi = uiRoutes.filter((r) => !uiRefs.has(r));
  const staleUi = [...uiRefs].filter((r) => !uiSet.has(r));
  const missingApi = apiRoutes.filter((r) => !apiRefs.has(r));
  const staleApi = [...apiRefs].filter((r) => !apiSet.has(r));
  return { appRefs, uiRefs, apiRefs, missingApps, staleApps, missingUi, staleUi, missingApi, staleApi };
}

function requireInstructionFiles(root = REPO_ROOT) {
  const needed = [CONTEXT_REL, SKILL_BROWSER_REL, SKILL_APPLIANCE_REL];
  const missing = needed.filter((rel) => !existsSync(join(root, rel)));
  return missing;
}

export function validateTemplateObject(obj, opts = {}) {
  const root = opts.root || REPO_ROOT;
  const errors = [];
  if (!obj || typeof obj !== "object") return { errors: ["checklist is not an object"], catalogIds: [], uiRoutes: [], apiRoutes: [] };
  if (obj.version !== VERSION) {
    return { errors: [`unsupported version: ${String(obj.version)} (want ${VERSION})`], catalogIds: [], uiRoutes: [], apiRoutes: [] };
  }
  if (!Array.isArray(obj.cases) || obj.cases.length === 0) errors.push("missing cases[]");
  const seen = new Set();
  for (const c of obj.cases || []) {
    if (c && typeof c.id === "string") {
      if (seen.has(c.id)) errors.push(`duplicate identifier: ${c.id}`);
      seen.add(c.id);
    }
    errors.push(...validateCaseFields(c));
  }
  errors.push(...checkSourcesExist(obj.cases || [], root).map((s) => `${s}`));
  const missingInstr = requireInstructionFiles(root);
  for (const m of missingInstr) errors.push(`missing instruction file: ${m}`);
  let catalogIds = [];
  let uiRoutes = [];
  let apiRoutes = [];
  try {
    catalogIds = opts.catalogIds || getCatalogIds(root);
  } catch (e) {
    errors.push(e.message);
  }
  try {
    uiRoutes = opts.uiRoutes || extractUiRoutes(root);
  } catch (e) {
    errors.push(e.message);
  }
  try {
    apiRoutes = opts.apiRoutes || extractApiRoutes(root);
  } catch (e) {
    errors.push(e.message);
  }
  if (errors.length === 0) {
    const cov = checkCoverage(obj.cases, { catalogIds, uiRoutes, apiRoutes });
    for (const id of cov.missingApps) errors.push(`missing application coverage: app:${id}`);
    for (const id of cov.staleApps) errors.push(`stale application reference: app:${id}`);
    for (const r of cov.missingUi) errors.push(`missing UI coverage: ui:${r}`);
    for (const r of cov.staleUi) errors.push(`stale UI reference: ui:${r}`);
    for (const r of cov.missingApi) errors.push(`missing API coverage: api:${r}`);
    for (const r of cov.staleApi) errors.push(`stale API reference: api:${r}`);
  }
  return { errors, catalogIds, uiRoutes, apiRoutes };
}

// --- run-record validation ---

export function validateRunObject(obj, runDir) {
  const errors = [];
  if (!obj || typeof obj !== "object") return ["run checklist is not an object"];
  if (obj.version !== VERSION) return [`unsupported version: ${String(obj.version)} (want ${VERSION})`];
  if (!obj.run || typeof obj.run !== "object") errors.push("missing run metadata");
  if (!Array.isArray(obj.cases) || obj.cases.length === 0) errors.push("missing cases[]");
  const seen = new Set();
  for (const c of obj.cases || []) {
    if (c && typeof c.id === "string") {
      if (seen.has(c.id)) errors.push(`duplicate identifier: ${c.id}`);
      seen.add(c.id);
    }
    errors.push(...validateCaseFields(c));
    if (!STATUSES.includes(c.status)) errors.push(`invalid status ${String(c.status)} for ${c.id}`);
    if (typeof c.owner !== "string") errors.push(`invalid owner for ${c.id}`);
    if (typeof c.actual !== "string") errors.push(`invalid actual for ${c.id}`);
    if (typeof c.reason !== "string") errors.push(`invalid reason for ${c.id}`);
    if (!Array.isArray(c.evidence)) errors.push(`invalid evidence for ${c.id}`);
    else {
      for (const ev of c.evidence) {
        if (!ev || typeof ev !== "object") {
          errors.push(`invalid evidence entry for ${c.id}`);
          continue;
        }
        if (!EVIDENCE_TYPES.includes(ev.type)) errors.push(`invalid evidence type ${String(ev.type)} for ${c.id}`);
        if (typeof ev.path !== "string" || ev.path.length === 0) {
          errors.push(`invalid evidence path for ${c.id}`);
          continue;
        }
        const norm = normalize(ev.path);
        if (norm.startsWith("..") || norm.startsWith("/") || norm.startsWith("\\") || /^[A-Za-z]:/.test(norm)) {
          errors.push(`unsafe evidence path for ${c.id}: ${ev.path}`);
          continue;
        }
        if (norm === "private" || norm.startsWith(`private${sep}`) || norm.startsWith("private/")) {
          errors.push(`evidence path inside private/ for ${c.id}: ${ev.path}`);
          continue;
        }
        if (c.status === "PASS" || c.status === "FAIL") {
          const abs = join(runDir, norm);
          if (!existsSync(abs) || !statSync(abs).isFile()) {
            errors.push(`missing evidence file for ${c.id}: ${ev.path}`);
          }
        }
      }
    }
    if ((c.status === "PASS" || c.status === "FAIL") && (!c.actual || c.actual.length === 0)) {
      errors.push(`missing actual for ${c.status} case ${c.id}`);
    }
    if ((c.status === "PASS" || c.status === "FAIL") && (!Array.isArray(c.evidence) || c.evidence.length === 0)) {
      errors.push(`missing evidence for ${c.status} case ${c.id}`);
    }
    if ((c.status === "BLOCKED" || c.status === "NOT_APPLICABLE") && (!c.reason || c.reason.length === 0)) {
      errors.push(`missing reason for ${c.status} case ${c.id}`);
    }
    if (c.status === "PASS" && Array.isArray(c.evidence_types) && c.evidence_types.includes("BROWSER")) {
      const hasBrowser = Array.isArray(c.evidence) && c.evidence.some((e) => e && e.type === "BROWSER");
      if (!hasBrowser) errors.push(`PASS case ${c.id} requires BROWSER evidence`);
    }
  }
  return errors;
}

// --- context bundle ---

export function buildContextBundle(root = REPO_ROOT) {
  const order = [AGENTS_REL, CONTEXT_REL, SKILL_BROWSER_REL, SKILL_APPLIANCE_REL];
  let out = "";
  for (const rel of order) {
    const abs = join(root, rel);
    if (!existsSync(abs)) throw new Error(`missing file: ${rel}`);
    out += `=== ${rel} ===\n`;
    out += readFileSync(abs, "utf8");
    if (!out.endsWith("\n")) out += "\n";
    out += "\n";
  }
  return out;
}

// --- rendering ---

export function renderChecklistMd(runObj) {
  const counts = { PENDING: 0, PASS: 0, FAIL: 0, BLOCKED: 0, NOT_APPLICABLE: 0 };
  for (const c of runObj.cases || []) {
    if (counts[c.status] !== undefined) counts[c.status]++;
  }
  const total = (runObj.cases || []).length;
  let md = `# Deep-test checklist\n\n`;
  md += `Run: ${runObj.run?.id || "unknown"}\n\n`;
  md += `Created: ${runObj.run?.created_at || "unknown"}\n\n`;
  md += `Source: ${runObj.run?.source_commit || "unknown"} (dirty: ${String(runObj.run?.dirty ?? "unknown")})\n\n`;
  md += `Total: ${total} | PASS: ${counts.PASS} | FAIL: ${counts.FAIL} | BLOCKED: ${counts.BLOCKED} | NOT_APPLICABLE: ${counts.NOT_APPLICABLE} | PENDING: ${counts.PENDING}\n\n`;
  md += `Validation success is not campaign success. PENDING means unattempted.\n\n`;
  for (const c of runObj.cases || []) {
    const box = c.status === "PASS" ? "[x]" : "[ ]";
    md += `- ${box} \`${c.id}\` — ${c.title} — ${c.status}\n`;
    md += `  - Area: ${c.area}\n`;
    md += `  - Covers: ${(c.covers || []).join(", ")}\n`;
    md += `  - Sources: ${(c.sources || []).join(", ")}\n`;
    md += `  - Requires: ${(c.requires || []).length ? c.requires.join(", ") : "none"}\n`;
    md += `  - Evidence types: ${(c.evidence_types || []).join(", ")}\n`;
    md += `  - Steps:\n`;
    for (const s of c.steps || []) md += `    - ${s}\n`;
    md += `  - Expected:\n`;
    for (const s of c.expected || []) md += `    - ${s}\n`;
    md += `  - Owner: ${c.owner || ""}\n`;
    md += `  - Actual: ${c.actual || ""}\n`;
    md += `  - Reason: ${c.reason || ""}\n`;
    if (Array.isArray(c.evidence) && c.evidence.length) {
      md += `  - Evidence:\n`;
      for (const e of c.evidence) md += `    - ${e.type}: ${e.path}\n`;
    } else {
      md += `  - Evidence: none\n`;
    }
    md += `\n`;
  }
  return md;
}

// --- path safety ---

function realDistDir() {
  const dist = join(REPO_ROOT, "dist");
  if (!existsSync(dist)) mkdirSync(dist, { recursive: true });
  return realpathSync(dist);
}

function resolveRunDir(outputArg) {
  const distReal = realDistDir();
  let target;
  if (outputArg) {
    target = resolve(REPO_ROOT, outputArg);
  } else {
    const ts = utcStamp(new Date());
    const commit = shortCommit() || "unversioned";
    target = resolve(REPO_ROOT, join("dist", "deep-test", `${ts}-${commit}`));
  }
  const normTarget = normalize(target);
  // Reject escapes through .. or outside dist/: compare normalized paths first.
  const distNorm = normalize(join(REPO_ROOT, "dist")) + sep;
  if (normTarget !== normalize(join(REPO_ROOT, "dist")) && !normTarget.startsWith(distNorm)) {
    throw new Error(`output path must be beneath dist/ (got ${outputArg || normTarget})`);
  }
  // Reject symlinked ancestors: realpath the nearest existing ancestor.
  let probe = normTarget;
  let suffix = "";
  while (!existsSync(probe) && probe !== dirname(probe)) {
    suffix = join(probe.slice(probe.lastIndexOf(sep) + 1), suffix);
    probe = dirname(probe);
  }
  let realProbe;
  try {
    realProbe = realpathSync(probe);
  } catch {
    throw new Error(`cannot resolve output path ancestors for ${outputArg || normTarget}`);
  }
  const realTarget = suffix ? join(realProbe, suffix) : realProbe;
  if (realTarget !== distReal && !realTarget.startsWith(distReal + sep)) {
    throw new Error(`output path escapes dist/ through symlink (got ${outputArg || normTarget})`);
  }
  return normTarget;
}

function utcStamp(d) {
  const p = (n, w = 2) => String(n).padStart(w, "0");
  return `${d.getUTCFullYear()}${p(d.getUTCMonth() + 1)}${p(d.getUTCDate())}T${p(d.getUTCHours())}${p(d.getUTCMinutes())}${p(d.getUTCSeconds())}${p(d.getUTCMilliseconds(), 3)}Z`;
}

function fullCommit() {
  try {
    return execSync("git rev-parse HEAD", { cwd: REPO_ROOT, encoding: "utf8" }).trim() || "unknown";
  } catch {
    return "unknown";
  }
}

function shortCommit() {
  try {
    const s = execSync("git rev-parse --short HEAD", { cwd: REPO_ROOT, encoding: "utf8" }).trim();
    return s || null;
  } catch {
    return null;
  }
}

function dirtyState() {
  try {
    const s = execSync("git status --porcelain", { cwd: REPO_ROOT, encoding: "utf8" });
    return s.trim().length > 0;
  } catch {
    return "unknown";
  }
}

// --- commands ---

function cmdContext() {
  try {
    process.stdout.write(buildContextBundle());
  } catch (e) {
    fail(e.message);
  }
}


function cmdCheck(runArg) {
  if (!runArg) {
    let template;
    try {
      template = JSON.parse(readFileSync(repoAbs(TEMPLATE_REL), "utf8"));
    } catch (e) {
      fail(`malformed template ${TEMPLATE_REL}: ${e.message}`);
    }
    const v = validateTemplateObject(template);
    if (v.errors.length) {
      for (const e of v.errors) err(e);
      fail(`check failed (${v.errors.length} problems)`);
    }
    process.stdout.write(`check PASS: ${template.cases.length} cases\n`);
    return;
  }
  const runDir = resolve(REPO_ROOT, runArg);
  const jsonPath = join(runDir, "checklist.json");
  if (!existsSync(jsonPath)) fail(`missing run file: ${join(runArg, "checklist.json")}`);
  let obj;
  try {
    obj = JSON.parse(readFileSync(jsonPath, "utf8"));
  } catch (e) {
    fail(`malformed run checklist: ${e.message}`);
  }
  const errors = validateRunObject(obj, runDir);
  if (errors.length) {
    for (const e of errors) err(e);
    fail(`check --run failed (${errors.length} problems)`);
  }
  const counts = { PENDING: 0, PASS: 0, FAIL: 0, BLOCKED: 0, NOT_APPLICABLE: 0 };
  for (const c of obj.cases) counts[c.status]++;
  process.stdout.write(`check --run PASS: ${obj.cases.length} cases (PENDING ${counts.PENDING}, PASS ${counts.PASS}, FAIL ${counts.FAIL}, BLOCKED ${counts.BLOCKED}, NOT_APPLICABLE ${counts.NOT_APPLICABLE})\n`);
  if (counts.PENDING > 0) process.stdout.write(`note: ${counts.PENDING} PENDING cases remain\n`);
}

function cmdRender(runArg) {
  if (!runArg) fail(`render requires --run <dir>`);
  const runDir = resolve(REPO_ROOT, runArg);
  const jsonPath = join(runDir, "checklist.json");
  const mdPath = join(runDir, "checklist.md");
  if (!existsSync(jsonPath)) fail(`missing run file: ${join(runArg, "checklist.json")}`);
  let obj;
  try {
    obj = JSON.parse(readFileSync(jsonPath, "utf8"));
  } catch (e) {
    fail(`malformed run checklist: ${e.message}`);
  }
  const errors = validateRunObject(obj, runDir);
  if (errors.length) {
    for (const e of errors) err(e);
    fail(`render refused: run record invalid (${errors.length} problems)`);
  }
  const md = renderChecklistMd(obj);
  const tmp = join(runDir, `.checklist.md.tmp-${process.pid}`);
  writeFileSync(tmp, md);
  renameSync(tmp, mdPath);
  process.stdout.write(`rendered: ${join(runArg, "checklist.md")}\n`);
}

function usage() {
  process.stderr.write(`usage:\n  node scripts/test-checklist.mjs context\n  node scripts/test-checklist.mjs new [--output <rel>]\n  node scripts/test-checklist.mjs check [--run <dir>]\n  node scripts/test-checklist.mjs render --run <dir>\n`);
}

async function main() {
  const argv = process.argv.slice(2);
  const cmd = argv[0];
  if (!cmd) {
    usage();
    process.exit(2);
  }
  if (cmd === "context") {
    if (argv.length !== 1) fail(`unknown arguments for context`);
    cmdContext();
    return;
  }
  if (cmd === "new") {
    let output = null;
    for (let i = 1; i < argv.length; i++) {
      if (argv[i] === "--output" && i + 1 < argv.length) {
        output = argv[++i];
      } else {
        fail(`unknown arguments for new (want --output <rel>)`);
      }
    }
    // chmod private/ needs fs chmod; patch cmdNew inline (it imports dynamically).
    cmdNewSync(output);
    return;
  }
  if (cmd === "check") {
    let run = null;
    for (let i = 1; i < argv.length; i++) {
      if (argv[i] === "--run" && i + 1 < argv.length) {
        run = argv[++i];
      } else {
        fail(`unknown arguments for check (want --run <dir>)`);
      }
    }
    cmdCheck(run);
    return;
  }
  if (cmd === "render") {
    let run = null;
    for (let i = 1; i < argv.length; i++) {
      if (argv[i] === "--run" && i + 1 < argv.length) {
        run = argv[++i];
      } else {
        fail(`unknown arguments for render (want --run <dir>)`);
      }
    }
    cmdRender(run);
    return;
  }
  usage();
  fail(`unknown command: ${cmd}`);
}

// Synchronous variant of cmdNew without the broken async chmod stub.
function cmdNewSync(outputArg) {
  let template;
  try {
    template = JSON.parse(readFileSync(repoAbs(TEMPLATE_REL), "utf8"));
  } catch (e) {
    fail(`malformed template ${TEMPLATE_REL}`);
  }
  const v = validateTemplateObject(template);
  if (v.errors.length) {
    for (const e of v.errors) err(e);
    fail(`template validation failed (${v.errors.length} problems)`);
  }
  let runDir;
  try {
    runDir = resolveRunDir(outputArg);
  } catch (e) {
    fail(e.message);
  }
  if (existsSync(runDir)) {
    fail(`output directory already exists: ${relative(REPO_ROOT, runDir)} (no overwrite)`);
  }
  const runId = runDir.split(sep).pop();
  const createdAt = new Date().toISOString();
  const commit = fullCommit();
  const dirty = dirtyState();
  const hashes = {
    template_sha256: sha256File(repoAbs(TEMPLATE_REL)),
    context: {
      [AGENTS_REL]: sha256File(repoAbs(AGENTS_REL)),
      [CONTEXT_REL]: sha256File(repoAbs(CONTEXT_REL)),
      [SKILL_BROWSER_REL]: sha256File(repoAbs(SKILL_BROWSER_REL)),
      [SKILL_APPLIANCE_REL]: sha256File(repoAbs(SKILL_APPLIANCE_REL)),
    },
    bundles_sha256: sha256File(repoAbs(BUNDLES_HASH_INPUT)),
    routes_sha256: sha256File(repoAbs(APP_TSX_REL)),
    api_sha256: sha256File(repoAbs(SERVER_GO_REL)),
  };
  const cases = template.cases.map((c) => ({
    ...c,
    status: "PENDING",
    owner: "",
    actual: "",
    evidence: [],
    reason: "",
  }));
  const runObj = {
    version: VERSION,
    run: { id: runId, created_at: createdAt, source_commit: commit, dirty, hashes },
    cases,
  };
  mkdirSync(runDir, { recursive: true });
  mkdirSync(join(runDir, "evidence"), { recursive: true });
  mkdirSync(join(runDir, "private"), { recursive: true });
  writeFileSync(join(runDir, "checklist.json"), JSON.stringify(runObj, null, 2) + "\n");
  const bundle = buildContextBundle();
  writeFileSync(join(runDir, "context.md"), bundle);
  writeFileSync(join(runDir, "checklist.md"), renderChecklistMd(runObj));
  try {
    chmodSync(join(runDir, "private"), 0o700);
  } catch {}
  process.stdout.write(bundle);
  if (!bundle.endsWith("\n")) process.stdout.write("\n");
  process.stdout.write(`run: ${relative(REPO_ROOT, runDir)}\n`);
  process.stdout.write(`context: ${relative(REPO_ROOT, join(runDir, "context.md"))}\n`);
  process.stdout.write(`checklist: ${relative(REPO_ROOT, join(runDir, "checklist.json"))}\n`);
  process.stdout.write(`rendered: ${relative(REPO_ROOT, join(runDir, "checklist.md"))}\n`);
  process.stdout.write(`cases: ${cases.length}\n`);
}

const isMain = process.argv[1] && resolve(process.argv[1]) === join(REPO_ROOT, "scripts", "test-checklist.mjs");
if (isMain) {
  await main();
}

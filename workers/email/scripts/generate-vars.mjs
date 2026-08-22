#!/usr/bin/env node
/**
 * Generate wrangler vars for the Omahab Email Worker.
 *
 * Derives the AI address (ai@<domain>) from the assistant slug and instance
 * domain at deploy time, and optionally creates a randomized recipient alias
 * as a shared secret between the Worker allowlist and omahabd ingestion.
 *
 * Usage:
 *   node scripts/generate-vars.mjs --domain example.com --slug ai [--alias] [--alias-length 8] [--out wrangler.toml] [--json]
 *   node scripts/generate-vars.mjs --domain example.com --slug custombot --alias --print-env
 *
 * Outputs:
 * - Updates [vars] in wrangler.toml (or prints JSON with --json)
 * - Prints the daemon env line: OMAHAB_EMAIL_RECIPIENT_ALIAS=<alias>
 *
 * The daemon reads the alias from:
 *   OMAHAB_EMAIL_RECIPIENT_ALIAS (env)  or
 *   OMAHAB_EMAIL_RECIPIENT_ALIAS_FILE (file containing the alias)
 * Both the Worker (RECIPIENT_ALIAS var) and the daemon must have the same
 * value; generation should be run once and the alias persisted.
 */

import { readFileSync, writeFileSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { resolve } from "node:path";

function parseArgs(argv) {
  const out = { domain: "", slug: "ai", alias: false, aliasLength: 8, json: false, printEnv: false, outPath: "" };
  for (let i = 2; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--domain" && i + 1 < argv.length) out.domain = argv[++i];
    else if (a === "--slug" && i + 1 < argv.length) out.slug = argv[++i];
    else if (a === "--alias") out.alias = true;
    else if (a === "--alias-length" && i + 1 < argv.length) out.aliasLength = Number(argv[++i]);
    else if (a === "--json") out.json = true;
    else if (a === "--print-env") out.printEnv = true;
    else if (a === "--out" && i + 1 < argv.length) out.outPath = argv[++i];
    else if (a === "--help" || a === "-h") {
      console.log(`Usage: generate-vars.mjs --domain <domain> --slug <slug> [--alias] [--alias-length 8] [--out wrangler.toml] [--json] [--print-env]`);
      process.exit(0);
    } else if (!a.startsWith("--") && !out.domain) {
      out.domain = a;
    }
  }
  return out;
}

function isValidDomain(d) {
  return /^[a-z0-9.-]+\.[a-z]{2,}$/i.test(d);
}
function isValidSlug(s) {
  return /^[a-z0-9_-]{1,32}$/i.test(s);
}

function generateAlias(slug, domain, length) {
  const hex = randomBytes(Math.ceil(length / 2)).toString("hex").slice(0, length);
  return `${slug}+${hex}@${domain}`;
}

function updateWranglerToml(content, primary, alias) {
  let updated = content;
  if (primary) {
    const re = /^ALLOWED_RECIPIENT\s*=.*$/m;
    if (re.test(updated)) updated = updated.replace(re, `ALLOWED_RECIPIENT = "${primary}"`);
    else updated = updated.replace(/\[vars\]/, `[vars]\nALLOWED_RECIPIENT = "${primary}"`);
  }
  if (alias) {
    const reAlias = /^#?\s*RECIPIENT_ALIAS\s*=.*$/m;
    if (reAlias.test(updated)) updated = updated.replace(reAlias, `RECIPIENT_ALIAS = "${alias}"`);
    else updated = updated.replace(/(ALLOWED_RECIPIENT\s*=.*)/, `$1\nRECIPIENT_ALIAS = "${alias}"`);
  } else {
    updated = updated.replace(/^RECIPIENT_ALIAS\s*=.*$/m, `# RECIPIENT_ALIAS = "ai+<random>@example.com"`);
  }
  return updated;
}

const args = parseArgs(process.argv);

if (!args.domain) {
  console.error("error: --domain <domain> is required (e.g., example.com)");
  process.exit(1);
}
args.domain = args.domain.trim().toLowerCase();
args.slug = args.slug.trim().toLowerCase();

if (!isValidDomain(args.domain)) {
  console.error(`error: invalid domain: ${args.domain}`);
  process.exit(1);
}
if (!isValidSlug(args.slug)) {
  console.error(`error: invalid slug: ${args.slug} (alphanum, _, -, 1-32 chars)`);
  process.exit(1);
}
if (!Number.isInteger(args.aliasLength) || args.aliasLength < 4 || args.aliasLength > 32) {
  console.error("error: --alias-length must be 4..32");
  process.exit(1);
}

const primary = `${args.slug}@${args.domain}`;
const alias = args.alias ? generateAlias(args.slug, args.domain, args.aliasLength) : null;

if (args.json) {
  const out = { ALLOWED_RECIPIENT: primary, INGEST_URL: "https://api.example.com/api/v1/email/ingest" };
  if (alias) out.RECIPIENT_ALIAS = alias;
  console.log(JSON.stringify(out, null, 2));
} else {
  console.log(`# Generated Omahab Email Worker vars`);
  console.log(`ALLOWED_RECIPIENT=${primary}`);
  if (alias) console.log(`RECIPIENT_ALIAS=${alias}`);
  console.log(`\n# Daemon env (set via systemd env or /etc/omahab/email-alias):`);
  if (alias) console.log(`OMAHAB_EMAIL_RECIPIENT_ALIAS=${alias}`);
  else console.log(`# No alias — only primary is accepted`);
  console.log(`\n# To apply, run:`);
  console.log(`#   node scripts/generate-vars.mjs --domain ${args.domain} --slug ${args.slug}${args.alias ? " --alias" : ""} --out wrangler.toml`);
  console.log(`#   wrangler deploy`);
  if (alias) console.log(`#   # also set on host: echo '${alias}' | sudo tee /var/lib/omahab/email-recipient-alias`);
  if (args.printEnv && alias) console.log(`\n${alias}`);
}

if (args.outPath) {
  const p = resolve(args.outPath);
  let content;
  try {
    content = readFileSync(p, "utf8");
  } catch {
    console.error(`warning: could not read ${p}, writing vars snippet instead`);
    const snippet = `[vars]\nALLOWED_RECIPIENT = "${primary}"\n${alias ? `RECIPIENT_ALIAS = "${alias}"` : "# RECIPIENT_ALIAS not configured"}\n`;
    writeFileSync(p, snippet, "utf8");
    process.exit(0);
  }
  const updated = updateWranglerToml(content, primary, alias);
  writeFileSync(p, updated, "utf8");
  console.log(`\nUpdated ${p}`);
}

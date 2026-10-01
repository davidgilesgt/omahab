# Omahab deep-test context

This file is the agent campaign bundle for deep end-to-end testing.
`node scripts/test-checklist.mjs context` prints this file with
`AGENTS.md` and both skill files, in fixed order, with source labels.
`node scripts/test-checklist.mjs new` snapshots this file into each run.

## Campaign order

1. Create a blank run with `node scripts/test-checklist.mjs new`.
2. Read the generated `context.md` before any assignment.
3. Record source commit, working-tree changes, ISO identity, installed
   generation, application versions, and browser version in the run.
4. Build the run inventory from the maintained checklist before app tests.
5. Reconcile source definitions with the installed system and visible menus.
6. If source discovery reveals missing cases, update the tracked template
   first, then generate a fresh blank checklist for the final campaign.
7. Never hide a new feature in an untracked private checklist.
8. Prepare one supervised installed appliance (section below).
9. First run setup with the coordinator and IdentityAndSetup.
10. After shared enrollment succeeds, run HouseholdApps and Development
    concurrently.
11. Run AIAndKnowledge after the coordinator configures providers and aliases.
12. Run CompanionAndMessages concurrently only on separate objects.
13. ClarityReview observes completed flows with its own browser sessions.
14. Run LifecycleAndRecovery only after other agents release the appliance.
15. Reconcile all agent results against the complete inventory.
16. Render the final checklist and write the final report.

Standard harness mode on a separate fresh disk verifies unchanged recovery
checks before the final interactive campaign.
If test-tool changes require commits, complete repository commit gates first.
Run the final ISO campaign from the resulting source state.
Run the recovery campaign (`scripts/e2e-iso-install.sh --recovery`,
`E2E_MEM_MB=10240`) only on a host with at least 12288 MiB available RAM.
It chains: capture (phrase, repository, custom bundle, scoped snapshot),
fresh-disk reinstall, full restore, selected subset, interrupt/resume at
a persisted stage, and activation rollback. The phrase and repository
password stay in the run's owner-only workdir directory; never echo them
into evidence, logs, or browser capture. Mark the campaign cases
blocked while capacity is short; never claim a VM proof that did not run.

Every administrator API request needs the panel bearer token, including
requests from the LAN, tailnet, and the ISO harness loopback tunnel.
The ISO harness reads the disposable token from the installed admin account
over SSH. Never copy that value into an evidence file or a browser log.

## Ownership

The coordinator owns the VM, credentials, shared configuration, and final
report. Product repairs are not part of a test run. Record failures before
any test-environment repair. Never change an expected result to make a
failure pass.

| Agent | Owned work |
|---|---|
| IdentityAndSetup | First-run browser setup, identities, enrollment, SSO, dashboard access |
| HouseholdApps | Photos, media, documents, bookmarks, home tiles, related user data |
| Development | Forgejo, Woodpecker, projects, ONCE releases, workspaces |
| AIAndKnowledge | LiteLLM, providers, Hermes, Hindsight, indexing, consent, MCP |
| CompanionAndMessages | Companion devices, environment distribution, sync, Drops, email, notifications |
| LifecycleAndRecovery | Installer, storage, maintenance, backup, restore, upgrades, interruption recovery |
| ClarityReview | Independent task completion, accessibility checks, administration measurements |

Each agent receives case identifiers, test-object prefixes, prerequisites,
and the evidence format. Agents skip builds, formatting, repository checks,
and shared configuration changes. The coordinator runs shared checks once.

Use separate browser contexts or processes for different identities.
Separate tabs alone do not isolate cookies. Use test-object prefixes
containing the run identifier and agent name. The coordinator alone changes
domain, global model mappings, exposure policy, recovery keys, server
maintenance, and shared services. Treat application restart, revocation,
migration, restore, and network changes as exclusive operations. Agents stop
dependent cases before an exclusive operation begins. Do not run heavy
indexing, CI builds, restores, and upgrades together. Return blocked cases
promptly so independent work can continue.

Use general-purpose task agents for interactive execution. Use read-only
scouts only for remaining source inventory work. Dispatch independent
runtime assignments together through `task`.

## Five-input contract

Only five user-supplied values exist, in an off-Git file outside the
repository:

`${XDG_CONFIG_HOME:-$HOME/.config}/omahab-test/credentials.json`

```json
{
  "domain": "",
  "cloudflare_a": "",
  "cloudflare_b": "",
  "tailscale_auth_key": "",
  "openrouter_api_key": ""
}
```

Require nonempty strings before the corresponding live tests. Never print
field values during validation. Reject symlinks, incorrect ownership,
group-readable files, and world-readable files. Reject any resolved
credential path inside the repository, including ignored directories. Do not
execute the file as shell code. Do not require a second settings file or
additional user-supplied identifiers, credentials, model names, or budgets.
Keep generated, non-secret run settings in the ignored run directory. Keep
generated credentials and recovery material in its private directory.
Checklist generation works without the credential file. Only live-test
preparation reads it.

Map `cloudflare_a` to `platform-app/cloudflare_dns`. Map `cloudflare_b` to
`platform-app/cloudflare_tunnel`. Key A needs `Zone:Read` and `DNS:Edit`
for the test zone. Key B is the Cloudflare API token, not a tunnel
connector token. Use setup reconciliation to create the connector token.
`refreshExposure` uses the Tunnel token for Access when no separate Access
token exists. If B lacks Access permissions, record affected sharing cases
as blocked. Never broaden token permissions automatically.

Derive zone and account identifiers through `cloudflare.ResolveZone`.
Exercise automatic discovery through the normal setup flow first. If the
user supplies a subdomain, query parent suffixes using A until the matching
authorized zone is found. Store derived identifiers through existing secret
APIs while preserving the original instance domain. Use only the supplied
domain and test-owned resources under it. Never replace existing DNS
records, Access applications, tunnels, or users without confirming test
ownership. Check for an existing tunnel named `omahab` before saving B.
Block external setup if that tunnel is not owned by this test.

Use the supplied Tailscale auth key for isolated test nodes only. A
short-lived, preauthorized, reusable ephemeral key permits fresh server and
client enrollment. Transfer the key into guest
`/run/omahab-test/tailscale.key` through protected standard input with
directory mode `0700` and file mode `0600`. Run
`sudo tailscale up --auth-key=file:/run/omahab-test/tailscale.key --timeout=110s`
inside the disposable guest. Never place the raw key in process arguments.
Remove the guest input file after enrollment. Do not add an auth-key field
to the product API or UI. Do not enroll the user desktop.

Use OpenRouter as the sole live model provider. Read key metadata from
`GET https://openrouter.ai/api/v1/key`. Read model metadata from
`GET https://openrouter.ai/api/v1/models/user`. Select compatible models
deterministically from current provider metadata; do not request model
names. Use lowest input-plus-output price first, then model identifier as
the tie-breaker. Use the key's existing provider-enforced cap. Permit paid
tests only when `limit_remaining` is positive, at most USD 5, and
`limit_reset` is null. All other key-limit states permit compatible
zero-price models only. Never raise the limit or purchase credits. Run one
model-producing scenario at a time with one synthetic user request. Stop
autonomous agent activity after three turns or 120 seconds. Record key usage
before and after each scenario. Stop model-producing applications when
credit is exhausted or the campaign uses USD 5.

Generate test identities, passkeys, SSH keys, recovery material, application
passwords, and all internal integration tokens. Generate test email
addresses under the supplied domain. Generate a password for a disposable
generic restic repository outside backed-up paths at
`/srv/omahab/backups/repos/omahab-test-backup`. Export the completed encrypted repository
into the host run directory before destructive recovery tests.

Keep Hetzner, other provider logins, external email delivery, and
unavailable hardware cases in the checklist. Mark their live cases blocked
when the five inputs cannot satisfy them.

## Browser tools

Use the available browser automation (`browser.open`, direct tab helpers,
`tab.run`, `observe`, screenshots) for every visible action. Use
`observe` before interaction and re-observe after navigation. Use `tab.run`
only where direct helpers are insufficient. Use Chromium desktop at
1440x900 and a narrow 390x844 viewport. Check keyboard-only operation,
visible focus, labels, dialog focus, contrast, and 200 percent zoom.

If browser tools are unavailable, mark all browser-required cases as
`BLOCKED` with reason `browser tools unavailable`, including the exact test
still required. Never substitute API-only evidence for a browser case and
mark it passed. A `PASS` whose required browser evidence is absent is
invalid, even when API evidence exists.

Pause screenshots and DOM capture during secret entry, invitation display,
and recovery display. Resume capture only after sensitive content is removed
or reliably masked.

## Evidence rules

Case states: `PENDING`, `PASS`, `FAIL`, `BLOCKED`, `NOT_APPLICABLE`.

Evidence types: `BROWSER`, `LIVE_API`, `LIVE_CLI`, `INTEGRATION`, `UNIT`,
`SOURCE_REVIEW`.

`SOURCE_REVIEW` and simulated tests cannot establish a live integration
pass. Each blocked case names the missing prerequisite and the exact test
still required. Each inapplicable case has a source-backed reason. Use
controlled browser request failure only for presentation-state checks and
label such evidence simulated. Use actual service interruption for
operational recovery checks.

Run outputs in each run directory:

- `context.md`: instruction snapshot
- `checklist.json`: authoritative run record with blank result fields
- `checklist.md`: human-readable checklist rendered from `checklist.json`
- `inventory.json`: features, routes, commands, tools, services, conditions
- `coverage.json`: tested and total counts per inventory group
- `findings.jsonl`: reproduction steps, expected behavior, impact, anchors
- `report.md`: findings by severity, limits, measurements, prerequisites
- `evidence/`: sanitized screenshots, observations, command results, logs
- `private/` (`0700`, files `0600`): secrets, recovery material, unsanitized evidence

Never distribute `private/` with the report. Never record authorization
headers, cookies, OAuth callbacks, token fragments, or secret-bearing
response bodies. The harness disposable administrator password appears in
its serial log; never use a real password there. Revoke test-generated
credentials after the run, including after failures. Do not revoke supplied
keys or remove the user credential file. Remove only test-owned Cloudflare
resources and generated guest credentials.

For every form, test empty input, invalid input, valid input, pending
state, error, and retry. Test duplicate submission when permitted. For
destructive dialogs, test Cancel, Escape, incorrect confirmation, correct
confirmation, and focus return. Test each list empty, populated, and
unavailable.

## Testing checklist maintenance

This requirement also appears in the root `AGENTS.md`. The generated agent
context carries it into every run.

- Add checklist cases when adding a feature or installed application.
- Update cases when changing behavior, access rules, failure handling, or
  recovery requirements.
- Remove obsolete cases when removing features.
- Update testing context and skills when their procedures or prerequisites
  change.
- Run `node scripts/test-checklist.mjs check` before considering the change
  complete.
- Use `node scripts/test-checklist.mjs new` to begin each independent
  campaign.
- Never replace the maintained template with a completed run checklist.
- Never commit supplied credentials, generated secrets, or run results.

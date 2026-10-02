#!/usr/bin/env bash
# Real local-restic backup → fresh-state recovery campaign for the ISO
# harness (scripts/e2e-iso-install.sh phase "recovery").
#
# This is the end-to-end recovery proof the unit/API tests cannot give:
# a real restic repository, the real scoped capture (quiesce, dumps, v2
# manifest), a fresh-disk reinstall, the real shared offline recovery
# executor (omahab-restore unit + `omahab recovery apply`), an
# interrupted-then-resumed restore, restored and excluded content
# assertions, and a custom bundle whose healthy service must answer the
# fresh health probe after recovery — all on the installed system, never
# against a booted-stale snapshot.
#
# Contract with the parent harness (all paths are host-side):
#   e2e_recovery_capture BASE TOKEN SSH_PORT SSH_KEY WORKDIR
#     -> performs recovery-key generate/confirm, configures a local
#        restic repository, installs the caddy app, writes the app-file
#        sentinel, adds + activates a custom bundle, runs a scoped
#        capture, and waits for the completed snapshot. Persists the
#        phrase/repo password under the workdir owner-only for the
#        later phases; echoes nothing on success.
#   e2e_recovery_restore BASE TOKEN SSH_PORT SSH_KEY WORKDIR
#     -> on the freshly reinstalled machine: stages + applies the real
#        recovery restore through the panel API, then asserts restored
#        sentinel, excluded content absent, custom bundle definition +
#        healthy service + fresh health probe, and setup checklist
#        restore=ok.
#   e2e_recovery_interrupt BASE TOKEN SSH_PORT SSH_KEY WORKDIR
#     -> stages a second restore, kills the live omahab-restore MainPID
#        at a persisted stage (never a wall-clock guess), proves the
#        journal/staging survived, then re-applies to completion.
#   e2e_recovery_rollback BASE TOKEN SSH_PORT SSH_KEY WORKDIR
#     -> after the recovery proof: poisons the recovered fixture's
#        module with a failing Nix activation script, triggers a real
#        bundle apply, requires the terminal rolled_back outcome, the
#        old generation restored, and panel + custom service health.
#
# All artifacts live under the harness workdir (dist/e2e by default).
# The parent also supplies E2E_EVIDENCE_DIR for secret-free browser evidence.
# The phrase never crosses argv: it is piped to the CLI over stdin or
# posted over the loopback-tunneled API, matching the product contract.
set -euo pipefail

# ---------------------------------------------------------------------------
# shared helpers
# ---------------------------------------------------------------------------

rc_log()  { printf '[e2e-recovery] %s\n' "$*"; }
rc_die()  { printf '[e2e-recovery] FATAL: %s\n' "$*" >&2; exit 1; }

# rc_api <method> <path> [json-body] — authenticated call through the
# SSH-tunneled panel base URL; echoes the response body.
rc_api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS --fail-with-body -m "${RC_API_TIMEOUT:-30}" -X "$method" -H "Authorization: Bearer $RC_TOKEN" "$RC_BASE$path")
  if [[ -n "$body" ]]; then
    printf '%s' "$body" | curl "${args[@]}" -H 'Content-Type: application/json' --data-binary @-
  else
    curl "${args[@]}"
  fi
}

# rc_api_code <method> <path> [json-body] — same but echoes the status code.
rc_api_code() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -o /dev/null -w '%{http_code}' -m 30 -X "$method" -H "Authorization: Bearer $RC_TOKEN" "$RC_BASE$path")
  if [[ -n "$body" ]]; then
    printf '%s' "$body" | curl "${args[@]}" -H 'Content-Type: application/json' --data-binary @-
  else
    curl "${args[@]}"
  fi
}


# rc_ssh_root <command...> — run a command as root in the guest. The
# admin account is in wheel and the install-time password is known to
# the harness (serial-log visibility matches the install phase); sudo
# reads it from stdin. Root is required to read the 0600 state files
# (journals, staging) and to observe/kill the restore unit.
RC_SUDO_PASS="${E2E_ADMIN_PASS:-e2e-test-pass-01}"
rc_ssh_root() {
  local quoted
  printf -v quoted '%q' "$*"
  printf '%s\n' "$RC_SUDO_PASS" "${RC_ROOT_INPUT:-}" | ssh -T -i "$RC_SSH_KEY" -p "$RC_SSH_PORT" \
    -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=15 \
    admin@127.0.0.1 "sudo -k -S -p '' bash -c $quoted" 2>/dev/null
}


# rc_setup_check <id> <status> — assert one setup checklist check state.
rc_setup_check() {
  local want_id="$1" want_status="$2"
  rc_api GET /api/v1/setup | python3 -c '
import json, sys
checks = json.load(sys.stdin)["checks"]
for c in checks:
    if c["id"] == sys.argv[1]:
        sys.exit(0 if c["status"] == sys.argv[2] else 1)
sys.exit(1)
' "$want_id" "$want_status"
}

# rc_wait_until <timeout-seconds> <description> <command...> — poll until
# the command succeeds (exit 0) or the timeout elapses. A terminal
# restore failure fails immediately with diagnostics.
rc_wait_until() {
  local timeout="$1" desc="$2"; shift 2
  local deadline=$(( SECONDS + timeout ))
  while (( SECONDS < deadline )); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    if [[ -n "$RC_OP_ID" ]]; then
      local failure
      failure="$(rc_restore_failure "$RC_OP_ID")"
      if [[ -n "$failure" ]]; then
        rc_restore_diagnose "$RC_OP_ID"
        rc_die "restore failed during: $desc ($failure)"
      fi
    fi
    sleep 3
  done
  [[ -z "${RC_OP_ID:-}" ]] || rc_restore_diagnose "$RC_OP_ID"
  rc_die "timed out waiting for: $desc"
}

# rc_restore_status <op-id> — echo the persisted operation status.
rc_restore_status() {
  rc_api GET "/api/v1/recovery/restore/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])'
}

# rc_restore_stage <op-id> — echo the persisted operation stage.
rc_restore_stage() {
  rc_api GET "/api/v1/recovery/restore/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["stage"])'
}

# rc_restore_failure <op-id> — echo the journaled error when the
# operation terminally failed; empty otherwise.
rc_restore_failure() {
  rc_api GET "/api/v1/recovery/restore/$1" | python3 -c 'import json,sys; op=json.load(sys.stdin); print(op.get("error","") if op.get("status")=="failed" else "")'
}

# rc_restore_diagnose <op-id> — dump unit state, unit journal, and the
# operation journal into the campaign log before a restore wait fails.
rc_restore_diagnose() {
  rc_log "restore diagnostics for $1:"
  rc_ssh_root 'systemctl status omahab-restore.service --no-pager || true; journalctl -u omahab-restore.service --no-pager -n 100 || true' \
    || rc_log "could not read the restore unit journal"
  rc_ssh_root "cat /var/lib/omahab/recovery-restore-operations/$1/journal.json 2>/dev/null || ls /var/lib/omahab/recovery-restore-operations 2>/dev/null || true" \
    || rc_log "could not read the operation journal"
  rc_ssh_root 'journalctl -u omahabd.service --no-pager -n 100 || true' \
    || rc_log "could not read the daemon journal"
}

# rc_restore_staged — the current restore operation is fully staged.
rc_restore_staged() {
  [[ "$(rc_restore_stage "$RC_OP_ID")" == "staged" && "$(rc_restore_status "$RC_OP_ID")" == "completed" ]]
}

# rc_wait_panel — wait for omahabd /up through the tunnel after the
# restore unit restarted it.
rc_wait_panel() {
  rc_wait_until 300 "panel /up after restore" \
    curl -sf -m 5 "$RC_BASE/up" -o /dev/null
}

# ---------------------------------------------------------------------------
# fixture: the custom bundle definition
# ---------------------------------------------------------------------------

# A real custom bundle: a tiny HTTP fixture service (python http.server
# standing in via a static index) on the reserved custom port 20871,
# gated on its appenv file exactly like curated bundles, with a real
# health_check the fresh health endpoint can probe. module.nix defines a
# genuine NixOS unit; the definition itself is part of the backup and
# must come back on the fresh machine through the recovery restore.
RC_BUNDLE_ID="e2e-fixture"
RC_BUNDLE_PORT="20871"
RC_BUNDLE_DATA_DIR="/var/lib/e2e-fixture"


# ---------------------------------------------------------------------------
# phase: capture (on the first installed boot)
# ---------------------------------------------------------------------------

e2e_recovery_capture() {
  RC_BASE="$1"; RC_TOKEN="$2"; RC_SSH_PORT="$3"; RC_SSH_KEY="$4"; local workdir="$5"
  local secrets_dir="$workdir/recovery-secrets"
  mkdir -p "$secrets_dir"
  chmod 700 "$secrets_dir"

  # --- 1. Recovery phrase: generate + confirm through the product flow.
  local gen
  gen="$(rc_api POST /api/v1/recovery/generate)" || rc_die "recovery generate failed"
  RC_PHRASE="$(printf '%s' "$gen" | python3 -c 'import json,sys; print(" ".join(json.load(sys.stdin)["phrase"]))')"
  local fp
  fp="$(printf '%s' "$gen" | python3 -c 'import json,sys; print(json.load(sys.stdin)["fingerprint"])')"
  [[ -n "$RC_PHRASE" && -n "$fp" ]] || rc_die "recovery generate returned no phrase/fingerprint"
  rc_api POST /api/v1/recovery/confirm \
    "{\"fingerprint\":\"$fp\",\"saved_ack\":true}" | grep -q '"confirmed":true' \
    || rc_die "recovery confirm failed"
  # Persist phrase + repo password owner-only for the later phases of THIS
  # harness run. Never argv, never a guest journal. Cleaned by the parent
  # default-clean trap together with the workdir.
  printf '%s\n' "$RC_PHRASE" > "$secrets_dir/phrase"
  rc_log "recovery phrase generated and confirmed (fingerprint $fp)"

  # --- 2. Local restic repository through the product repository flow.
  #    Location is outside every backup source tree (dataDir, not the
  #    system state dir) per the module's local-repo contract; the
  #    password is a harness secret stored only in the workdir.
  RC_REPO_PASS="$(python3 -c 'import secrets; print(secrets.token_hex(24))')"
  printf '%s\n' "$RC_REPO_PASS" > "$secrets_dir/repo-pass"
  chmod 600 "$secrets_dir/phrase" "$secrets_dir/repo-pass"
  local repo_path="/srv/omahab/backups/repos/e2e-recovery"
  local repo_json
  repo_json="$(rc_api POST /api/v1/backup-repositories \
    "{\"label\":\"e2e-recovery\",\"location\":\"$repo_path\",\"password\":\"$RC_REPO_PASS\"}")" \
    || rc_die "backup repository configure failed"
  RC_REPO_ID="$(printf '%s' "$repo_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  [[ -n "$RC_REPO_ID" ]] || rc_die "backup repository configure returned no id"
  rc_log "local restic repository configured ($repo_path)"
  local source_instance
  source_instance="$(rc_api GET /api/v1/status | python3 -c 'import json,sys; print(json.load(sys.stdin)["instance_id"])')"
  printf '%s/%s\n' "$repo_path" "$source_instance" > "$secrets_dir/source-location"
  chmod 600 "$secrets_dir/source-location"

  # --- 3. Install the caddy app through the product flow: its native
  #    data volume /var/lib/caddy lives OUTSIDE the omahab state dir, so
  #    the app-file sentinel proves app payload restore, not
  #    server-config restore.
  local code
  code="$(rc_api_code POST /api/v1/applications '{"bundle_id":"caddy","exposure":"private"}')"
  [[ "$code" == "201" || "$code" == "409" ]] || rc_die "caddy install failed (HTTP $code)"
  rc_log "caddy application installed"

  # --- 4. App-file sentinel inside the captured caddy data volume, and
  #    an excluded sentinel inside a directory the capture excludes
  #    (.cache is on the global exclude list): after recovery the first
  #    must exist, the second must NOT — proving scope selection rather
  #    than a whole-disk copy.
  rc_ssh_root 'mkdir -p /var/lib/caddy/e2e && printf "e2e-app-sentinel-v1\n" > /var/lib/caddy/e2e/sentinel.txt' \
    || rc_die "could not write app sentinel"
  rc_ssh_root 'mkdir -p /var/lib/caddy/.cache && printf "must-not-come-back\n" > /var/lib/caddy/.cache/excluded-sentinel.txt' \
    || rc_die "could not write excluded sentinel"
  rc_log "app sentinel written (caddy data volume, outside stateDir)"

  # --- 5. Custom bundle definition through the product flow: added via
  #    the API (validated, committed) and activated with the real
  #    bundle apply (nixos-rebuild in the omahab-bundle-apply unit), so
  #    the running service exists on the source machine and its
  #    definition + data scope travel inside the snapshot.
  rc_ssh_root "mkdir -p $RC_BUNDLE_DATA_DIR && printf 'fixture-index-v1\n' > $RC_BUNDLE_DATA_DIR/index.html" \
    || rc_die "could not write fixture data"
  # Build the create-bundle JSON payload with embedded documents
  # (bundle.json + module.nix) in pure python: no nested shell quoting.
  bundle_payload="$(python3 - "$RC_BUNDLE_ID" "$RC_BUNDLE_PORT" "$RC_BUNDLE_DATA_DIR" <<'PYEOF'
import json, sys

bundle_id, port, data_dir = sys.argv[1], int(sys.argv[2]), sys.argv[3]
bundle_json = json.dumps({
    "id": bundle_id,
    "name": "E2E Recovery Fixture",
    "port": port,
    "default_exposure": "private",
    "max_exposure": "private",
    "health_check": {"kind": "http", "path": "/", "port": port},
    "startup_grace_seconds": 30,
    "oidc": {"supported": False},
    "default": False,
    "route": "",
    "units": ["e2e-fixture.service"],
    "groups": ["admins"],
    "data": [{"name": "files", "path": data_dir}],
}, indent=2)
module_nix = """# E2E recovery fixture - a real (tiny) custom bundle service whose
# health the fresh bundle-health endpoint must probe after recovery.
{ config, lib, pkgs, ... }:
with lib;
let
  stateDir = "/var/lib/omahab";
  appEnv = "${stateDir}/appenv";
in
{
  config = mkIf config.services.omahab.enable {
    systemd.services.e2e-fixture = {
      description = "E2E recovery fixture service";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      serviceConfig = {
        Type = "simple";
        EnvironmentFile = [ "-${appEnv}/%s.env" ];
        ExecStart = "${pkgs.python3}/bin/python3 -m http.server %d --bind 127.0.0.1 --directory %s";
        StateDirectory = "%s";
        Restart = "on-failure";
      };
      unitConfig.ConditionPathExists = [
        "${appEnv}/%s.env"
        "!${stateDir}/maintenance/apps/%s"
      ];
    };
  };
}
""" % (bundle_id, port, data_dir, bundle_id, bundle_id, bundle_id)
print(json.dumps({"id": bundle_id, "bundle_json": bundle_json, "module_nix": module_nix}))
PYEOF
)"
  local add_code
  add_code="$(printf '%s' "$bundle_payload" | curl -s -o /dev/null -w '%{http_code}' -m 30 \
    -X POST -H "Authorization: Bearer $RC_TOKEN" -H 'Content-Type: application/json' \
    --data-binary @- "$RC_BASE/api/v1/bundles")"
  [[ "$add_code" == "201" ]] || rc_die "custom bundle add failed (HTTP $add_code)"
  rc_log "custom bundle $RC_BUNDLE_ID added via API (committed)"
  # Seed the environment gate before apply health checks run. No
  # application row exists yet: apply must probe the actual service.
  rc_ssh_root 'mkdir -p /var/lib/omahab/appenv && printf "E2E=1\n" > /var/lib/omahab/appenv/e2e-fixture.env && chmod 600 /var/lib/omahab/appenv/e2e-fixture.env' \
    || rc_die "could not prepare fixture environment"

  # Activate through the real browser action; the campaign then verifies
  # the completed switch and the healthy custom service.
  local apply_op
  apply_op="$(printf '%s' "$RC_TOKEN" | node web/e2e/installed.mjs "$RC_BASE" "${E2E_EVIDENCE_DIR:?parent harness evidence directory required}" apply)" \
    || rc_die "browser bundle apply trigger failed"
  RC_APPLY_ID="$(printf '%s' "$apply_op" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  [[ -n "$RC_APPLY_ID" ]] || rc_die "bundle apply returned no operation id"
  rc_apply_wait 2400 completed "bundle apply to activate the custom bundle"
  rc_log "custom bundle activated (real nixos-rebuild switch, operation $RC_APPLY_ID)"
  # After the switch, omahabd restarts; make sure the panel is back.
  rc_wait_panel
  rc_wait_until 60 "fixture service healthy" rc_fixture_probe
  [[ "$(rc_api_code POST "/api/v1/bundles/$RC_BUNDLE_ID/health")" == "204" ]] \
    || rc_die "fresh custom health probe required an application row"
  code="$(rc_api_code POST /api/v1/applications "{\"bundle_id\":\"$RC_BUNDLE_ID\"}")"
  [[ "$code" == "201" ]] || rc_die "fixture installation for scoped capture failed (HTTP $code)"
  rc_ssh_root 'mkdir -p /srv/omahab/sync && printf "captured-sync-v1\n" > /srv/omahab/sync/e2e-scope.txt' \
    || rc_die "could not prepare server-sync scope"

  # --- 6. Scoped capture through the product flow: caddy (app scope)
  #    plus the custom bundle's scope and server-config. The excluded
  #    .cache sentinel must stay out via the capture excludes.
  local capture
  capture="$(RC_API_TIMEOUT=900 rc_api POST /api/v1/backups "{\"repository\":\"$RC_REPO_ID\",\"scopes\":[\"server-config\",\"app:caddy\",\"app:$RC_BUNDLE_ID\",\"server-sync\"]}")" \
    || rc_die "backup create failed"
  local snapshot
  snapshot="$(printf '%s' "$capture" | python3 -c '
import json, sys
run = json.load(sys.stdin)
if run["status"] != "completed" or not run.get("snapshot_id"):
    raise SystemExit("Scoped capture did not complete")
print(run["snapshot_id"])
')" || rc_die "capture did not complete"
  local source_location
  source_location="$(cat "$secrets_dir/source-location")"
  RC_ROOT_INPUT="$RC_REPO_PASS" rc_ssh_root "read -r RESTIC_PASSWORD; export RESTIC_PASSWORD; restic -r '$source_location' dump '$snapshot' /var/lib/omahab/backup-staging/manifest.json" | python3 -c '
import json, sys
manifest = json.load(sys.stdin)
expected = {"server-config", "app:caddy", "app:" + sys.argv[1], "server-sync"}
sys.exit(0 if expected <= {scope["name"] for scope in manifest["scopes"]} else 1)
' "$RC_BUNDLE_ID" || rc_die "saved snapshot omitted requested scopes"
  rc_log "scoped capture completed with verified snapshot manifest"

  # A post-capture file must not appear on the fresh restored machine.
  rc_ssh_root "printf 'post-capture-must-not-return\n' > $RC_BUNDLE_DATA_DIR/after-capture.txt" \
    || rc_die "could not write post-capture sentinel"

  rc_log "capture phase complete"
}

# rc_apply_wait <timeout-seconds> <want-status> <description> — poll the
# bundle apply operation until it reaches the wanted terminal status.
# Any other terminal outcome (completed|failed|rolled_back) fails the
# campaign immediately with the operation id, status, and stage instead
# of waiting out the timeout. The switch inside an apply restarts
# omahabd, so a poll that cannot reach the API yields no body — an
# unavailable curl result is never fed to the JSON parser.
rc_apply_wait() {
  local timeout="$1" want="$2" desc="$3"
  local deadline=$(( SECONDS + timeout )) body status stage
  while (( SECONDS < deadline )); do
    body="$(rc_api GET "/api/v1/bundles/apply/$RC_APPLY_ID" 2>/dev/null || true)"
    if [[ -n "$body" ]]; then
      read -r status stage <<< "$(printf '%s' "$body" | python3 -c '
import json, sys
try:
    op = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
print(op.get("status", ""), op.get("stage", ""))
' 2>/dev/null || true)"
      case "$status" in
        completed|failed|rolled_back)
          if [[ "$status" == "$want" ]]; then
            return 0
          fi
          rc_die "bundle apply operation $RC_APPLY_ID ended as $status at stage '$stage' (wanted $want): $desc"
          ;;
      esac
    fi
    sleep 3
  done
  rc_die "timed out waiting for: $desc"
}

# rc_fixture_probe — the custom bundle's service answers its health
# check in the guest (through the fresh-probe endpoint OR the port
# directly; used pre-capture for readiness, post-restore via the API
# probe for the acceptance).
rc_fixture_probe() {
  rc_ssh_root "curl -sf -m 5 http://127.0.0.1:$RC_BUNDLE_PORT/ >/dev/null"
}


# ---------------------------------------------------------------------------
# phase: restore (on the freshly reinstalled machine)
# ---------------------------------------------------------------------------

e2e_recovery_restore() {
  RC_BASE="$1"; RC_TOKEN="$2"; RC_SSH_PORT="$3"; RC_SSH_KEY="$4"; local workdir="$5"
  local secrets_dir="$workdir/recovery-secrets"
  RC_PHRASE="$(cat "$secrets_dir/phrase")"
  RC_REPO_PASS="$(cat "$secrets_dir/repo-pass")"
  local repo_path
  repo_path="$(cat "$secrets_dir/source-location")"

  # --- 1. Fresh-machine posture: no custom bundle, no caddy app, no
  #    sentinel, restore check pending.
  rc_api GET /api/v1/bundles | grep -q "\"id\":\"$RC_BUNDLE_ID\"" \
    && rc_die "fresh machine already lists the custom bundle (stale state?)"
  rc_ssh_root 'test -e /var/lib/caddy/e2e/sentinel.txt' \
    && rc_die "fresh machine already holds the app sentinel (stale state?)"
  rc_log "fresh state confirmed (no custom bundle, no sentinel)"

  # --- 2. Real recovery: stage through the daemon (listing, extraction,
  #    validation, journal), then trigger the omahab-restore unit that
  #    runs the shared offline executor as root with omahabd stopped.
  local stage_json
  stage_json="$(rc_api POST /api/v1/recovery/restore \
    "{\"phrase\":\"$RC_PHRASE\",\"location\":\"$repo_path\",\"password\":\"$RC_REPO_PASS\",\"include_apps\":true}")" \
    || rc_die "recovery restore staging failed"
  RC_OP_ID="$(printf '%s' "$stage_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  [[ -n "$RC_OP_ID" ]] || rc_die "recovery restore staging returned no operation id"
  rc_wait_until 600 "restore staging (extract+validate)" rc_restore_staged
  rc_log "restore staged and validated (operation $RC_OP_ID)"

  # Trigger the real unit path: phrase travels via the tmpfs handoff,
  # the unit stops omahabd, applies the swap, restarts the daemon.
  rc_api POST "/api/v1/recovery/restore/$RC_OP_ID/apply" \
    "{\"phrase\":\"$RC_PHRASE\"}" >/dev/null || rc_die "restore apply trigger failed"
  rc_wait_until 1800 "recovery restore to complete" rc_restore_done
  rc_wait_panel
  rc_log "recovery restore completed (operation $RC_OP_ID)"

  # --- 3. Restored content: the app-file sentinel from the caddy data
  #    volume (outside stateDir) is back with the exact bytes.
  local got
  got="$(rc_ssh_root 'cat /var/lib/caddy/e2e/sentinel.txt')" \
    || rc_die "restored machine lacks the caddy app sentinel"
  [[ "$got" == "e2e-app-sentinel-v1" ]] || rc_die "sentinel content mismatch: '$got'"
  [[ "$(rc_ssh_root "cat $RC_BUNDLE_DATA_DIR/index.html")" == "fixture-index-v1" ]] \
    || rc_die "custom application data was not restored"
  [[ "$(rc_ssh_root 'cat /srv/omahab/sync/e2e-scope.txt')" == "captured-sync-v1" ]] \
    || rc_die "full recovery did not restore server-sync"

  # --- 4. Excluded content: the .cache sentinel (capture exclude) and
  #    the post-capture file (content equality) must both be absent.
  rc_ssh_root 'test -e /var/lib/caddy/.cache/excluded-sentinel.txt' \
    && rc_die "excluded .cache sentinel came back in the restore"
  rc_ssh_root "test -e $RC_BUNDLE_DATA_DIR/after-capture.txt" \
    && rc_die "post-capture file came back in the restore (content equality violated)"
  rc_log "restored content verified; excluded and post-capture content absent"

  # --- 5. Custom bundle: definition restored into the bundles repo,
  #    listed with source=custom, and its service healthy through a
  #    fresh health probe.
  rc_ssh_root "test -f /var/lib/omahab/bundles/$RC_BUNDLE_ID/bundle.json" \
    || rc_die "custom bundle definition not restored into the bundles repo"
  rc_api GET /api/v1/bundles | python3 -c '
import json, sys
items = json.load(sys.stdin)["items"]
row = next((i for i in items if i.get("id") == sys.argv[1]), None)
sys.exit(0 if row and row.get("source") == "custom" else 1)
' "$RC_BUNDLE_ID" || rc_die "restored bundle list does not show $RC_BUNDLE_ID as custom"
  local probe_code
  probe_code="$(rc_api_code POST "/api/v1/bundles/$RC_BUNDLE_ID/health")"
  [[ "$probe_code" == "204" ]] \
    || rc_die "fresh health probe of restored custom bundle failed (HTTP $probe_code, want 204)"
  rc_log "custom bundle restored, listed as custom, and healthy via the fresh probe"

  # --- 6. Final setup state: the restore checklist item is ok.
  rc_setup_check restore ok || rc_die "setup checklist restore check is not ok after recovery"
  rc_log "setup checklist records restore=ok"
  rc_log "restore phase complete"
}

rc_restore_done() {
  [[ "$(rc_restore_stage "$RC_OP_ID")" == "completed" && "$(rc_restore_status "$RC_OP_ID")" == "completed" ]]
}

# Restore only selected scopes over changed live data. The captured but
# unselected server-sync scope must remain exactly as the operator left it.
e2e_recovery_subset() {
  RC_BASE="$1"; RC_TOKEN="$2"; RC_SSH_PORT="$3"; RC_SSH_KEY="$4"; local workdir="$5"
  RC_PHRASE="$(cat "$workdir/recovery-secrets/phrase")"
  RC_REPO_PASS="$(cat "$workdir/recovery-secrets/repo-pass")"
  local repo_path
  repo_path="$(cat "$workdir/recovery-secrets/source-location")"
  rc_ssh_root 'rm -f /var/lib/caddy/e2e/sentinel.txt && printf "excluded-scope-change\n" > /srv/omahab/sync/e2e-scope.txt' \
    || rc_die "could not change subset proof data"
  local staged
  staged="$(rc_api POST /api/v1/recovery/restore \
    "{\"phrase\":\"$RC_PHRASE\",\"location\":\"$repo_path\",\"password\":\"$RC_REPO_PASS\",\"include_apps\":true,\"scopes\":[\"server-config\",\"app:caddy\",\"app:$RC_BUNDLE_ID\"]}")" \
    || rc_die "selected restore staging failed"
  RC_OP_ID="$(printf '%s' "$staged" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  rc_wait_until 600 "selected restore staging" rc_restore_staged
  rc_api POST "/api/v1/recovery/restore/$RC_OP_ID/apply" "{\"phrase\":\"$RC_PHRASE\"}" >/dev/null \
    || rc_die "selected restore apply failed"
  rc_wait_until 1800 "selected restore completion" rc_restore_done
  rc_wait_panel
  [[ "$(rc_ssh_root 'cat /var/lib/caddy/e2e/sentinel.txt')" == "e2e-app-sentinel-v1" ]] \
    || rc_die "selected app scope did not restore its data"
  [[ "$(rc_ssh_root 'cat /srv/omahab/sync/e2e-scope.txt')" == "excluded-scope-change" ]] \
    || rc_die "unselected captured scope was changed"
  [[ "$(rc_api_code POST "/api/v1/bundles/$RC_BUNDLE_ID/health")" == "204" ]] \
    || rc_die "custom service did not resume after selected restore"
  rc_log "selected scope restored; unselected captured scope unchanged"
}

# ---------------------------------------------------------------------------
# phase: interruption + resume
# ---------------------------------------------------------------------------

e2e_recovery_interrupt() {
  RC_BASE="$1"; RC_TOKEN="$2"; RC_SSH_PORT="$3"; RC_SSH_KEY="$4"; local workdir="$5"
  local secrets_dir="$workdir/recovery-secrets"
  RC_PHRASE="$(cat "$secrets_dir/phrase")"
  RC_REPO_PASS="$(cat "$secrets_dir/repo-pass")"
  local repo_path
  repo_path="$(cat "$secrets_dir/source-location")"

  # --- 1. Stage a second restore from the same snapshot: this
  #    re-stages extraction and validation, then applies over the
  #    already-restored machine (idempotent for control state).
  local stage_json
  stage_json="$(rc_api POST /api/v1/recovery/restore \
    "{\"phrase\":\"$RC_PHRASE\",\"location\":\"$repo_path\",\"password\":\"$RC_REPO_PASS\",\"include_apps\":true}")" \
    || rc_die "second restore staging failed"
  RC_OP_ID="$(printf '%s' "$stage_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  rc_wait_until 600 "second restore staging" rc_restore_staged
  rc_api POST "/api/v1/recovery/restore/$RC_OP_ID/apply" \
    "{\"phrase\":\"$RC_PHRASE\"}" >/dev/null || rc_die "second apply trigger failed"

  # --- 2. Interrupt at a PERSISTED stage: watch the journal until the
  #    operation reports the swapping/apps stage and the omahab-restore
  #    unit has a live MainPID, then SIGKILL that PID. A missed window
  #    (operation completes before the kill lands) FAILS the campaign —
  #    never counts as passed.
  local killed=0 stage_seen=""
  local deadline=$(( SECONDS + 900 ))
  while (( SECONDS < deadline )); do
    local stage pid
    stage="$(rc_restore_stage "$RC_OP_ID" 2>/dev/null || true)"
    pid="$(rc_ssh_root "systemctl show -p MainPID --value omahab-restore.service" 2>/dev/null || true)"
    if [[ "$pid" =~ ^[0-9]+$ && "$pid" != "0" ]]; then
      case "$stage" in
        swapping|apps|unit-started)
          stage_seen="$stage"
          if rc_ssh_root "kill -9 $pid"; then
            killed=1
            break
          fi
          ;;
      esac
    fi
    # The operation may complete while we poll: then the interruption
    # window was missed -> hard failure, not a pass.
    if [[ -z "$stage_seen" ]] && rc_restore_done; then
      rc_die "restore completed before the interruption window opened (kill missed)"
    fi
    sleep 2
  done
  [[ $killed -eq 1 ]] || rc_die "never observed a live omahab-restore MainPID at stage '$stage_seen' to kill (interruption not proved)"
  rc_log "restore interrupted: SIGKILL on MainPID at persisted stage '$stage_seen'"

  # --- 3. Survived state: journal + staging preserved, not cleaned.
  rc_ssh_root "test -d /var/lib/omahab/recovery-restore-staging" \
    || rc_die "staging tree removed after interruption"
  rc_ssh_root "test -f /var/lib/omahab/recovery-restore-operations/$RC_OP_ID/journal.json" \
    || rc_die "operation journal removed after interruption"
  rc_log "journal and staging preserved across the interruption"

  # --- 4. Resume: re-apply the same operation — the shared executor
  #    detects the journaled in-flight state and resumes from the
  #    persisted stage, never redoing completed stages.
  rc_wait_panel
  rc_api POST "/api/v1/recovery/restore/$RC_OP_ID/apply" \
    "{\"phrase\":\"$RC_PHRASE\"}" >/dev/null || rc_die "resume apply trigger failed"
  rc_wait_until 1800 "resumed restore to complete" rc_restore_done
  rc_wait_panel
  rc_log "resumed restore completed (operation $RC_OP_ID)"

  # --- 5. Post-resume acceptance: sentinel still intact, custom bundle
  #    still healthy through the fresh probe, checklist still ok.
  local got
  got="$(rc_ssh_root 'cat /var/lib/caddy/e2e/sentinel.txt')" \
    || rc_die "sentinel lost after the interrupted+resumed restore"
  [[ "$got" == "e2e-app-sentinel-v1" ]] || rc_die "sentinel mismatch after resume: '$got'"
  [[ "$(rc_ssh_root 'cat /srv/omahab/sync/e2e-scope.txt')" == "captured-sync-v1" ]] \
    || rc_die "full resumed restore did not restore server-sync"
  local probe_code
  probe_code="$(rc_api_code POST "/api/v1/bundles/$RC_BUNDLE_ID/health")"
  [[ "$probe_code" == "204" ]] || rc_die "custom bundle unhealthy after resume (HTTP $probe_code)"
  rc_setup_check restore ok || rc_die "setup checklist restore check not ok after resume"
  rc_log "post-resume acceptance passed"
  rc_log "interrupt phase complete"
}

# ---------------------------------------------------------------------------
# phase: generation-activation rollback (Switch-error path)
# ---------------------------------------------------------------------------

# A focused real rollback proof using the recovered custom fixture:
# record the running generation, edit the fixture's module.nix to add a
# valid Nix activation script that exits nonzero AFTER activation
# begins (systemdtmpfiles/activation snippet), trigger the real bundle
# apply, and require the terminal rolled_back outcome with the old
# generation restored, the panel back, and the custom service still
# healthy through the fresh probe. This exercises the Switch-error
# rollback path of the apply executor — not a build failure (which
# never switches) and not a mocked hook. The poisoned fixture lives
# only in the disposable guest.
e2e_recovery_rollback() {
  RC_BASE="$1"; RC_TOKEN="$2"; RC_SSH_PORT="$3"; RC_SSH_KEY="$4"; local workdir="$5"

  # --- 1. Record the running generation and the fixture's committed
  #    definition hash (for restore afterwards).
  local gen_before
  gen_before="$(rc_ssh_root 'readlink /run/current-system')" \
    || rc_die "could not read the running generation"
  [[ -n "$gen_before" ]] || rc_die "empty generation path"
  rc_log "running generation: $gen_before"

  # --- 2. Poison the fixture module: a valid NixOS activation snippet
  #    that fails during activation (after the switch begins). The
  #    definition is edited in the committed custom-bundles repo so the
  #    apply pins and builds this exact revision.
  rc_ssh_root "cp /var/lib/omahab/bundles/$RC_BUNDLE_ID/module.nix /var/lib/omahab/bundles/$RC_BUNDLE_ID/module-clean.nix
cat > /var/lib/omahab/bundles/$RC_BUNDLE_ID/module.nix <<'NIXEOF'
{ lib, ... }:
{
  imports = [ ./module-clean.nix ];
  system.activationScripts.e2eRollbackPoison = lib.stringAfter [ \"etc\" ] ''
    echo \"e2e rollback poison: failing activation deliberately\" >&2
    false
  '';
}
NIXEOF
cd /var/lib/omahab/bundles && git add -A && git commit -m 'e2e: poison fixture activation for rollback proof' >/dev/null" \
    || rc_die "could not poison the fixture module"
  rc_log "fixture module poisoned with a failing activation script (committed)"

  # --- 3. Trigger the real apply: build succeeds (the poison is a
  #    valid Nix expression), the switch starts activating and fails.
  local apply_op
  apply_op="$(rc_api POST /api/v1/bundles/apply '{}')" \
    || rc_die "rollback apply trigger failed"
  RC_APPLY_ID="$(printf '%s' "$apply_op" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  [[ -n "$RC_APPLY_ID" ]] || rc_die "rollback apply returned no operation id"
  rc_log "bundle apply triggered (operation $RC_APPLY_ID); waiting for the terminal outcome"

  # --- 4. Require the terminal rolled_back outcome (field-based
  #    contract: status rolled_back, never just an error string).
  rc_apply_wait 2400 rolled_back "rolled_back terminal outcome"
  rc_log "apply terminated as rolled_back (operation $RC_APPLY_ID)"

  # --- 5. Old generation restored: the running system is the recorded
  #    generation again, the panel is serving, and the recovered custom
  #    service still answers the fresh health probe.
  rc_wait_panel
  local gen_after
  gen_after="$(rc_ssh_root 'readlink /run/current-system')" \
    || rc_die "could not read the post-rollback generation"
  [[ "$gen_after" == "$gen_before" ]] \
    || rc_die "rollback did not restore the old generation ($gen_after != $gen_before)"
  local probe_code
  probe_code="$(rc_api_code POST "/api/v1/bundles/$RC_BUNDLE_ID/health")"
  [[ "$probe_code" == "204" ]] \
    || rc_die "custom bundle unhealthy after rollback (HTTP $probe_code, want 204)"
  rc_log "old generation restored; panel and custom service healthy after rollback"

  # --- 6. Restore the clean fixture definition in the guest so later
  #    operators (or a --keep inspection) are not left with the poison.
  rc_ssh_root "cd /var/lib/omahab/bundles && git revert --no-edit HEAD >/dev/null 2>&1 || git reset --hard HEAD~1" \
    || rc_die "could not restore the clean fixture definition"
  rc_log "rollback phase complete"
}

# ---------------------------------------------------------------------------
# dispatch
# ---------------------------------------------------------------------------

case "${1:-}" in
  capture)   shift; e2e_recovery_capture "$@" ;;
  restore)   shift; e2e_recovery_restore "$@" ;;
  subset)    shift; e2e_recovery_subset "$@" ;;
  interrupt) shift; e2e_recovery_interrupt "$@" ;;
  rollback)  shift; e2e_recovery_rollback "$@" ;;
  *) sed -n '2,38p' "$0"; exit 2 ;;
esac

# vim: set ts=2 sw=2 et:

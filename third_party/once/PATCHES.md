# ONCE Fork Patches (Omahab)

This is a narrow fork of `https://github.com/basecamp/once` at tag `v0.3.2` (commit `3d6e539331c3c4fd6738a3e276f660546d63c467`) vendored at `third_party/once` via `git subtree`. Module path remains `github.com/basecamp/once` and it is built as its own module (`third_party/once/go.mod`), not added to the root `go.mod`.

Applied patches (suitable for upstreaming):

1. **`deploy --proxy-bind <addr>` — loopback proxy bind** (`internal/command/deploy.go`, `internal/command/settings_flags.go`, `internal/docker/proxy.go`)
   - `deploy` accepts `--proxy-bind 127.0.0.1:8080` (or any `host:port`).
   - `proxy.Boot` publishes `127.0.0.1:<port>:80` only; no `443` publish when loopback bind is set. `kamal-proxy run` still gets `--metrics-port` but HTTP port binding is at the Docker publish level, which ONCE controls. `proxy.deployArgs` is unchanged (proxy itself has no bind flag).

2. **`--tls external` — disable internal TLS** (`internal/command/deploy.go`, `internal/command/settings_flags.go`, `internal/docker/proxy.go`)
   - `deploy` accepts `--tls external` (string, default `external` for Omahab; `internal` preserves old `--disable-tls` behavior).
   - When `--tls external`, `ApplicationSettings.DisableTLS` is forced true and `proxy.deployArgs` never adds `--tls`. Caddy owns external TLS.

3. **`--json` structured output** (`internal/command/deploy.go`, `internal/command/status.go`, `internal/command/remove.go` (undeploy), `internal/command/list.go`)
   - `deploy`, `status`, `undeploy` (`remove` alias), and `list` accept `--json`.
   - On `--json`, the command prints machine-readable JSON to stdout and suppresses TUI progress:
     - `deploy --json` → `{"version":"<image-digest-or-tag>","status":"ok"}` or `{"error":"...","status":"error"}` on failure.
     - `status --json` → `{"healthy":bool,"detail":"...","status":"ok"}`.
     - `undeploy --json` → `{"status":"ok"}`.
     - `list --json` → `[{"name":"...","host":"...","running":bool}]`.

4. **`--secrets-file <path>` — KEY=VAL env injection** (`internal/command/deploy.go`, `internal/command/settings_flags.go`)
   - `deploy` accepts `--secrets-file <path>`.
   - File is read as `KEY=VAL` lines (trim whitespace, ignore blank/`#` comments, last wins on duplicate), merged into `ApplicationSettings.EnvVars` alongside `--env` (also last wins). File contents are never logged; only the path appears in errors.

5. **`status --app <name> --json` — machine-readable health** (`internal/command/status.go` (new), `internal/command/root.go`)
   - New `status` command: `once status --app <name> [--proxy-bind <addr>] [--json]`.
   - Looks up application by `Name` (Omahab slug) or `Host`, then probes via `application.verifyHTTP` or returns `{"healthy":false}` if not found. JSON shape matches `internal/controlplane/once.go` expectation: `{"healthy":bool,"detail":string,"status":string}`.

6. **`undeploy --app <name> --hostname <host> --json` — app removal** (`internal/command/remove.go`, `internal/command/undeploy.go` (alias), `internal/command/root.go`)
   - `remove` (aliased as `undeploy` for Omahab) accepts `--app` and `--hostname` plus `--json`.
   - Resolves by `--app` (Name) first, then `--hostname` (Host), calls `app.Remove`/`proxy.Remove`, prints JSON on `--json`. Retains `remove <host>` positional for backward compat.

7. **`--image` with `@sha256:` digest** (`internal/docker/application_settings.go`)
   - Already upstream via `distribution/reference`; no diff. Validated that `image@sha256:<64hex>` parses.

Upstreamability: patches 1–6 are flag additions and JSON output that do not break existing `once` usage (`--host`, `--disable-tls`, interactive TUI). Flag defaults preserve old behavior (no proxy-bind → 0.0.0.0:80/443; no secrets-file → no extra env; no --json → human output).

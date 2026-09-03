# Omahab companion daemon protocol (omahab-clientd)

Single source of truth for the Unix-domain socket contract between the server, the Go daemon (`omahab-clientd`), and all shell plugins (Omarchy Quickshell QML, future macOS Swift `NSStatusItem`). Any implementation that speaks this file can replace the reference QML without forking business logic.

## Transport

- **Socket:** `AF_UNIX`, `SOCK_STREAM`, mode `0600`.
- **Path precedence** (canonical, defined in `internal/client/config.go:DefaultSocketPath` and re-exported by `internal/apiclient`):
  1. `$OMAHAB_CLIENTD_SOCKET` if set (non-empty).
  2. `$XDG_RUNTIME_DIR/omahab-clientd.sock` if `XDG_RUNTIME_DIR` set.
  3. `/run/user/<uid>/omahab-clientd.sock` if `/run/user/<uid>` exists.
  4. `$HOME/.cache/omahab/clientd.sock` if home detectable.
  5. `/tmp/omahab-clientd-<uid>.sock` fallback.
- **Framing:** newline-delimited JSON (NDJSON), one JSON object per line, UTF-8, `1 MiB` max per message. The daemon reads until a complete valid JSON object is buffered and `Buffered()==0`; clients should flush after the trailing `\n`.
- **Concurrency:** one request per connection; server closes after one response. For `subscribe` (future push, see C1) the connection is held open and the daemon streams `{"event":"status","data":DaemonStatus}` frames — this is not yet required for A1, `subscribe` currently returns `{"result":"subscribed"}` and closes.
- **Deadlines:** daemon sets a `10s` read deadline per connection; clients should set a matching write+read deadline (typed Go client uses `10s` or context deadline).

## Wire types

```go
// Request — client -> daemon
type SocketRequest struct {
  ID     string         `json:"id"`               // client-chosen correlation id, echoed back
  Method string         `json:"method"`           // canonical method name, lower-cased, trimmed
  Params map[string]any `json:"params,omitempty"` // method-specific
}

// Response — daemon -> client
type SocketResponse struct {
  ID     string       `json:"id"`               // echoes Request.ID
  Result any          `json:"result,omitempty"`
  Error  *SocketError `json:"error,omitempty"`
}
type SocketError struct {
  Code    string `json:"code"`    // stable, see § Error codes
  Message string `json:"message"` // human, no secrets
}
```

`ID` may be any non-empty string; the daemon echoes it verbatim. `Method` is case-insensitive, normalized with `strings.TrimSpace(strings.ToLower(...))`.

Legacy envelope `{"action": "...", "params": {...}}` is **not** supported. A legacy envelope has no `method` key, so `Method == ""` after decode and the daemon returns `unknown_method` (see Acceptance: `printf '{"action":"status"}' | socat ...` returns `unknown_method`).

## Error codes

- `bad_request` — missing/invalid params or invalid JSON.
- `not_found` — e.g. `project.clone` slug not found on server.
- `conflict` — e.g. `project.clone` destination already exists.
- `internal` — server unreachable, git failure, launcher failure, etc. Message is safe to surface (redacted).
- `unknown_method` — method not in canonical set. Shape: `{"error":{"code":"unknown_method","message":"unknown method \"...\""}}`.
- `not_found` is also used for `xai.oauth.connect` removed path — now `unknown_method`.

HTTP-style errors from the server are mapped to `internal` with the server's message. The daemon never leaks tokens, env values, or raw git output beyond `git clone failed: ...` (truncated).

## Canonical method table

One name per action; aliases from pre-A1 (`open-ai`, `hermes.open`, `project_clone`, `runner.*`, `env.sync`, etc.) are **removed**. Clients must use exactly the names below.

| Method | Params | Result | Errors | Description |
|---|---|---|---|---|
| `status` | `{}` | `DaemonStatus` (§ Status) | `internal` | Current daemon status (online, version, events, env, backup, projects). |
| `diagnose` | `{}` | `DiagnosticReport` (`internal/client/diagnostics.go`) | `internal` | Runs Tailscale/DNS/TLS/PocketID/instance checks. Same as `omahab doctor --json`. |
| `ai.open` | `{"url"?: string}` | `{"result":"opened ai"}` | `internal` | Opens Hermes/AI URL. If `url` omitted, derives from `ServerURL`. Preflight checks instance pinning. |
| `dashboard.open` | `{}` | `{"result":"opened omahab"}` | `internal` | Opens `ServerURL` in default browser. |
| `project.list` | `{}` | `[]ProjectState` | — | Local project fetch states (`Project`, `LocalPath`, `LastFetched`, `FetchError`, `GitStatus`). Synced from `GET /api/v1/companion/projects`. |
| `project.clone` | `{"slug": string, "dir"?: string}`<br>`slug` may be project ID or slug. `dir` defaults to `~/Projects/<slug>`. | `{"project_id": string, "slug": string, "dir": string, "clone_url": string}` | `bad_request` (slug required), `not_found`, `conflict` (dest exists), `internal` (not connected, no repo URL, `git clone` failed, terminal failed) | Resolves project via `GET /api/v1/companion/projects` (device token), `git clone <repository_url> <dir>`, `Upsert`s `ProjectStore`, then `OpenTerminal(dir)`. Requires device enrollment (`oma_dev_`). Uses `HOME` for default dir. |
| `project.open` | `{"slug": string}` or `{"project_id": string}` | `{"project_id": string, "dir": string}` | `bad_request`, `internal` | Opens project dir in terminal. Resolves `LocalPath` from `ProjectStore`, else `~/Projects/<slug>`. |
| `workspace.list` | `{}` | `[]Workspace` (`domain.Workspace`) | `internal` (not connected) | Proxies `GET /api/v1/companion/workspaces` (device auth). |
| `workspace.create` | `{"project_slug": string, "title": string, "instructions"?: string}`<br>`project_slug` also accepts `slug` alias for compat. | `Workspace` | `bad_request`, `internal` | Creates via `POST /api/v1/companion/workspaces` (device auth), then if `running|pending` opens `ssh -t omahab@<host> sudo omahab workspace attach <id>` in terminal. `host` from `ServerURL`. |
| `workspace.attach` | `{"id": string}` or `{"workspace_id": string}` or `{"dir": string, "local_path"?: string}` | `{"workspace_id": string, "result":"terminal opened"}` | `internal` | If `id` present, `ssh -t omahab@<host> sudo omahab workspace attach <id>`; else `OpenTerminal(dir \| "." )`. `workspace` is canonical, `runner` alias removed. |
| `workspace.stop` | `{"id": string}` or `{"workspace_id": string}` | `{"workspace_id": string, "result":"stopped"}` | `bad_request`, `internal` | Calls `POST /api/v1/companion/workspaces/{id}/stop` (device auth, reuses `workspaces.Service.Stop`). |
| `environment.sync` | `{}` | `{"result":"environment synced","detail":"Applied to new apps; restart existing apps"}` | `internal` | `EnvironmentManager.Sync` — fetches `GET /api/v1/companion/environment` (If-None-Match), atomic `0600`, D-Bus `SetEnvironment`. |
| `environment.clear` | `{}` | `{"result":"environment cleared"}` | `internal` | Removes managed file and unsets via D-Bus. |
| `environment.status` | `{}` | `{"revision": int, "variable_count": int, "synced_at": *time.Time, "error": string}` | — | Redacted; never includes values. |
| `backup.run` | `{}` | `{"result":"backup completed"}` | `internal` | Runs `backupDrive` (`RunBackupDrive`) synchronously (10m ctx). |
| `backup.status` | `{}` | `{"last_snapshot": *time.Time, "error": string}` | — | Refreshes via `StatusBackupDrive`, returns cached `backupLastSnapshot`/`backupError`. |
| `subscribe` | `{}` | `{"result":"subscribed"}` | — | Placeholder for C1 push. Future: holds connection and streams `{"event":"status","data":DaemonStatus}` on every state change, no deadline. Currently returns immediately and closes. |

`project.new` / `sync.add` / `xai.oauth.connect` are **deleted**. `sync.add` will return `unknown_method` until C5 re-adds it with real Syncthing enrollment. `xai.oauth.connect` is removed; OAuth is via `omahab provider login xai` + loopback relay.

### Status shape (`DaemonStatus`)

```json
{
  "online": true,
  "instance_id": "...",
  "version": "0.1.0",
  "health": "healthy",
  "server_url": "https://omahab.example.com",
  "events": [ {"id":"...","type":"agent.awaiting_approval", "severity":"info", "message":"...", "read_at": null, ...} ],
  "unread_count": 3,
  "unread_events": 3,
  "active_runners": 1,
  "waiting_agents": 1,
  "sync_conflicts": 0,
  "projects": [ {"project": {...}, "local_path": "...", "last_fetched": "...", "fetch_error": "", "git_status": ""} ],
  "last_sync_at": "2026-09-03T00:00:00Z",
  "error": "",
  "environment_revision": 2,
  "environment_variable_count": 5,
  "environment_synced_at": "2026-09-03T00:00:00Z",
  "environment_error": "",
  "has_xai_oauth_session": false,
  "backup_last_snapshot": "2026-09-03T00:00:00Z",
  "backup_error": ""
}
```

Field names are `snake_case` as stored; QML also handles `camelCase` aliases for backwards compat but new clients should use `snake_case`.

## Examples

Status request (canonical):

```sh
printf '{"id":"1","method":"status"}\n' | socat - UNIX-CONNECT:$XDG_RUNTIME_DIR/omahab-clientd.sock
# -> {"id":"1","result":{"online":true,...}}
```

Legacy envelope (must return unknown_method):

```sh
printf '{"id":"1","action":"status"}\n' | socat - UNIX-CONNECT:$XDG_RUNTIME_DIR/omahab-clientd.sock
# -> {"id":"1","error":{"code":"unknown_method","message":"unknown method \"\""}} 
# or {"error":{"code":"unknown_method","message":"unknown method \"\""}} if ID missing
printf '{"id":"2","method":"project.clone","params":{"slug":"my-app"}}\n' | socat - ...
# clones https://git.example.com/omahab/my-app.git -> ~/Projects/my-app and opens terminal
printf '{"id":"3","method":"workspace.stop","params":{"id":"abc123"}}\n' | socat - ...
# -> {"id":"3","result":{"workspace_id":"abc123","result":"stopped"}}
```

Unknown method:

```sh
printf '{"id":"99","method":"sync.add","params":{"name":"Notes"}}\n' | socat - ...
# -> {"id":"99","error":{"code":"unknown_method","message":"unknown method \"sync.add\""}}
```

Typed Go client (`internal/apiclient`):

```go
c := apiclient.NewClientdClient("") // uses DefaultSocketPath
var st client.DaemonStatus
if err := c.Call(ctx, "status", nil, &st); err != nil {
  var ce *apiclient.ClientdError
  if errors.As(err, &ce) && ce.Code == "unknown_method" { /* ... */ }
}
if err := c.Call(ctx, "project.clone", map[string]any{"slug":"my-app"}, nil); err != nil { /* ... */ }
if err := c.Call(ctx, "workspace.stop", map[string]any{"id": id}, nil); err != nil { /* ... */ }
```

## QML usage (Omarchy)

```qml
// Clientd.qml
readonly property string socketPath: Quickshell.env("OMAHAB_CLIENTD_SOCKET") || (Quickshell.env("XDG_RUNTIME_DIR") + "/omahab-clientd.sock")
Socket { path: root.socketPath; ... parser: SplitParser { splitMarker: "\n"; onRead: handleResponse } }
function refresh() { enqueue("status", {}, "status", ""); enqueue("workspace.list", {}, "workspace_list", ""); ... }
function workspaceStop(id) { enqueue("workspace.stop", {id: id}, "action", "Stop workspace") }
```

*No* `OMAHAB_SOCKET` — renamed to `OMAHAB_CLIENTD_SOCKET` in A1. `Clientd.qml` call sites use `status`, `diagnose`, `ai.open`, `dashboard.open`, `project.list`, `project.clone`, `project.open`, `workspace.list`, `workspace.create`, `workspace.attach`, `workspace.stop`, `environment.*`, `backup.*`, `subscribe`.

## Device HTTP API (complementary)

The daemon proxies companion actions to the server via device-authenticated endpoints (all `Authorization: Bearer oma_dev_...`, rejected on admin routes with 403):

- `GET /api/v1/companion/projects` — used by `project.clone` to resolve `slug` → `repository_url`.
- `GET /api/v1/companion/workspaces` / `POST /api/v1/companion/workspaces` / `POST /api/v1/companion/workspaces/{id}/stop` — used by `workspace.*`.
- `GET /api/v1/companion/environment` — used by `environment.*`.
- `POST /api/v1/provider-oauth/xai/callback/{session_id}` — loopback relay (not via daemon).

Admin token (`Bearer` without `oma_dev_` prefix) is rejected on these with `403`. Unknown device method maps to `404`/`403` on HTTP, which the daemon surfaces as `internal`.

## Versioning

`DaemonStatus.version` echoes the server's version (from `omahabd`); `omahab --version` on the device should match. Socket protocol is versioned by the `method` set; additions are additive, removals are breaking and require a major version bump and `unknown_method` fallback.

## Security notes

- Socket is `0600` and parent dir `0700`; never world-readable.
- Daemon never logs `params` values, env values, device tokens, or `git clone` credentials. `project.clone` redacts `clone_url` only via safe `clone_url` in result (no token; per-device token is via `~/.git-credentials` helper in workspaces, not needed for local clone unless Forgejo requires auth — clone URL is as stored in DB, which for private repos needs the device's `oma_dev_` token via credential helper; A2's `project.clone` currently does a plain `git clone <url>` and relies on the server's `repository_url` being accessible via tailnet without extra auth, or fails with `internal: git clone failed` — C4 will add per-device Forgejo token injection).
- `backup.*` values are never returned in error messages.

## Reference implementation

- `internal/client/daemon.go:dispatchSocket` — canonical handler.
- `internal/apiclient/clientd.go:Call` — typed NDJSON client.
- `internal/client/config.go:DefaultSocketPath` — socket path.
- `companion/omarchy/Clientd.qml` — QML client.

## Testing the contract

```sh
go vet ./internal/client/... ./internal/apiclient/... ./cmd/...
# grep for dead HTTP facade must be empty:
grep -rn '"/v1/' internal/apiclient  # -> no output
# socket smoke:
printf '{"id":"1","method":"status"}\n' | socat - UNIX-CONNECT:$HOME/.cache/omahab/clientd.sock
printf '{"id":"1","action":"status"}\n' | socat - UNIX-CONNECT:$HOME/.cache/omahab/clientd.sock
# second line must contain unknown_method
```

# Userdata Local

MuxCore household userdata sidecar module.

Durable per-user library state for the household loop: watch progress / resume position, continue-watching rows, favorites, and client preferences. Persists data in local SQLite; exposes gRPC for mesh consumers and HTTP compatible with `/api/userdata` clients.

Tracked by https://github.com/Muxcore-Media/umbrella/issues/19 · Security: https://github.com/Muxcore-Media/umbrella/issues/32

---

## How it works

```
media-ui / native clients ──HTTP (Bearer + user header)──► userdata-local (:9701) ──► SQLite
core / BFF ──────────────── gRPC (mTLS or session) ─────► userdata-local (:9703) ──► SQLite
                                      │
                                      └── validates sessions via auth-local (:9403)
```

Each household user has one JSON blob (`progress`, `favorites`, `prefs`, `playlists`, `queue`). Field names match `muxcore-ios` `UserDataModels.swift` and `media-ui-app` userdata helpers.

---

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `USERDATA_LOCAL_GRPC_ADDR` | `:9703` | gRPC listen address |
| `USERDATA_LOCAL_HTTP_ADDR` | `:9701` | HTTP listen address |
| `USERDATA_LOCAL_DB_PATH` | `~/.muxcore/userdata.db` | SQLite database file |
| `AUTH_LOCAL_GRPC_ADDR` | `localhost:9403` | auth-local gRPC address for session validation |
| `MUXCORE_GRPC_ADDR` | — | Core mesh address |
| `MUXCORE_MODULE_ID` | `userdata-local` | Module ID override |
| `MUXCORE_TLS_CERT` / `MUXCORE_TLS_KEY` / `MUXCORE_TLS_CA` | — | Mesh mTLS material (production) |
| `USERDATA_TLS_CERT` / `USERDATA_TLS_KEY` / `USERDATA_TLS_CA` | — | gRPC listener TLS overrides |
| `MUXCORE_INSECURE_DISABLE_TLS` | — | Dev-only plaintext gRPC (`true`) |

---

## Authentication

### HTTP

Clients must send **both**:

1. `Authorization: Bearer <session-token>` — validated via auth-local
2. `X-MuxCore-User-Id: <household-user-id>` — must match the validated session (unless caller has `admin` role)

Requests with a spoofed user id or missing/invalid bearer token receive `401` / `403`.

### gRPC

- **Mesh callers** (verified mTLS client cert CN, e.g. `muxcore`): may access any `user_id`.
- **User callers**: supply `x-auth-token` or `authorization: Bearer` metadata; `user_id` must match the session (or caller is `admin`).

See `SECURITY.md` for the full threat model.

---

## API summary

### gRPC (`muxcore.userdata.v1.UserDataService`)

| RPC | Description |
|-----|-------------|
| `Get` | Load full userdata blob for `user_id` |
| `Put` | Merge incoming JSON blob for `user_id` |
| `ListContinueWatching` | Filter in-progress items (`limit`, default 24) |

Proto: `proto/muxcore/userdata/v1/userdata.proto`

### HTTP

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/userdata` | Return merged blob for authenticated user |
| `PUT` | `/api/userdata` | Merge request JSON blob; return merged blob |
| `GET` | `/health` | Unauthenticated health check |

### Blob sections (in scope)

| Key | Purpose |
|-----|---------|
| `progress` | Resume position, watched flag, continue-watching source |
| `favorites` | Starred titles |
| `prefs` | Display/home/playback preferences (stored for sync) |
| `playlists`, `queue` | Pass-through sections for client sync |

Progress merge uses `updatedAt` (newer wins). Watched auto-detection uses the 92% threshold from native clients.

---

## Quick start

```bash
make build
# auth-local must be running (default :9403)
./userdata-local --muxcore-mesh-addr localhost:9090
```

Dev core: `MUXCORE_INSECURE_DISABLE_TLS=true ./muxcored` in `../core`.

Example authenticated HTTP request:

```bash
curl -H "Authorization: Bearer $SESSION" \
     -H "X-MuxCore-User-Id: alice" \
     http://localhost:9701/api/userdata
```

---

## Capability

| Capability | Role |
|------------|------|
| `userdata.local` | Canonical household userdata store |
| `settings` | Module settings surface (`db_path`) |

---

## Tests

```bash
go test ./...
```

Offline SQLite/fixture tests only (mock auth provider; no live auth-local required).

## License

GPL-3.0

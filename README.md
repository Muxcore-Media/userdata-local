# Userdata Local

MuxCore household userdata sidecar module.

Durable per-user library state for the household loop: watch progress / resume position, continue-watching rows, favorites, and client preferences. Persists data in local SQLite; exposes gRPC for mesh consumers and HTTP compatible with `/api/userdata` clients.

Tracked by https://github.com/Muxcore-Media/umbrella/issues/19

---

## How it works

```
media-ui / native clients ──HTTP──► userdata-local (:9701) ──► SQLite
core / BFF ──────────────── gRPC ──► userdata-local (:9703) ──► SQLite
```

Each household user has one JSON blob (`progress`, `favorites`, `prefs`, `playlists`, `queue`). Field names match `muxcore-ios` `UserDataModels.swift` and `media-ui-app` userdata helpers.

---

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `USERDATA_LOCAL_GRPC_ADDR` | `:9703` | gRPC listen address |
| `USERDATA_LOCAL_HTTP_ADDR` | `:9701` | HTTP listen address |
| `USERDATA_LOCAL_DB_PATH` | `~/.muxcore/userdata.db` | SQLite database file |
| `MUXCORE_GRPC_ADDR` | — | Core mesh address |
| `MUXCORE_MODULE_ID` | `userdata-local` | Module ID override |

HTTP callers must send `X-MuxCore-User-Id` with the authenticated household user id.

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
| `GET` | `/api/userdata` | Return merged blob for header user |
| `PUT` | `/api/userdata` | Merge request JSON blob; return merged blob |

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
./userdata-local --muxcore-mesh-addr localhost:9090
```

Dev core: `MUXCORE_INSECURE_DISABLE_TLS=true ./muxcored` in `../core`.

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

Offline SQLite/fixture tests only.

## License

GPL-3.0

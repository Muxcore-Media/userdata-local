# AGENTS.md — userdata-local

MuxCore sidecar module (`userdata-local`).

## Module identity

| Field | Value |
|-------|-------|
| Directory | `userdata-local` |
| Capabilities | `userdata.local`, `settings` |
| Contracts | none declared (blob JSON matches muxcore-ios / media-ui-app) |

## Agent rules

- Modules run as gRPC sidecars; capabilities are the security boundary.
- TLS required in production (`MUXCORE_INSECURE_DISABLE_TLS` is dev-only).
- Match existing Go patterns; run `gofmt` and `go test ./...` before finishing.
- Offline/fixture tests only — no live download or pirate services.
- `contracts-media` has no userdata event types yet; blob schema is documented in README.

## Settings / environment

| Variable | Default | Description |
|----------|---------|-------------|
| `USERDATA_LOCAL_GRPC_ADDR` | `:9703` | gRPC listen address |
| `USERDATA_LOCAL_HTTP_ADDR` | `:9701` | HTTP listen address (`GET/PUT /api/userdata`) |
| `USERDATA_LOCAL_DB_PATH` | `~/.muxcore/userdata.db` | SQLite database path |
| `AUTH_LOCAL_GRPC_ADDR` | `localhost:9403` | auth-local gRPC for session validation |
| `MUXCORE_GRPC_ADDR` | — | Core mesh address |
| `MUXCORE_MODULE_ID` | `userdata-local` | Module ID override |

HTTP requests require `Authorization: Bearer <session>` validated via auth-local and `X-MuxCore-User-Id` matching the authenticated household user id.

## Build

```bash
cd userdata-local
go test ./...
make build
```

## Internal API

- gRPC: `muxcore.userdata.v1.UserDataService` (`Get`, `Put`, `ListContinueWatching`)
- HTTP: `GET/PUT /api/userdata` with JSON blob body
- Store: SQLite table `user_blobs` keyed by `user_id`

Tracked by https://github.com/Muxcore-Media/umbrella/issues/19

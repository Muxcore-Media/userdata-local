# Security

Report security issues privately to the MuxCore maintainers.

## Data at rest

This module stores household viewing metadata locally in SQLite. Protect `USERDATA_LOCAL_DB_PATH` with restrictive file permissions (`0700` on the parent directory is created at init).

## Authentication and authorization

userdata-local does **not** trust client-supplied identity headers or gRPC `user_id` fields alone.

### HTTP (`/api/userdata`)

- Requires `Authorization: Bearer <session>` validated via **auth-local** (`AuthService.Validate`).
- Requires `X-MuxCore-User-Id` to match the validated session user id.
- Users with the `admin` role may access other household user ids.
- `/health` remains unauthenticated.

### gRPC (`UserDataService`)

Two caller models (same pattern as auth-local):

1. **Mesh service account** — verified mTLS client certificate CN (and optional `x-caller-id` metadata that must match the cert CN). Trusted mesh peers (e.g. muxcore, BFF) may access any `user_id`.
2. **User session** — `x-auth-token` or `authorization: Bearer` metadata validated via auth-local; `user_id` must match the session principal (or caller is `admin`).

Spoofed `X-MuxCore-User-Id` / `user_id` without a valid session are rejected.

### Authoritative parental policy (`/api/parental-policy`)

This separate, currently unused provider resource follows umbrella ADR-0030.
Every GET/PUT requires a freshly validated `Authorization: Bearer` session.
The legacy userdata mesh-certificate and `x-auth-token` paths do not authorize
policy access. Only current admins may write; self reads and admin reads of
other accounts are confined to the verified tenant. Other target accounts must
exist in auth-local. Client tenant headers, query parameters and blob contents
cannot establish policy scope. The empty tenant is a household scope, not a
cross-tenant administrator grant. This is resource-local isolation, not full
product multi-tenancy.

Policies use their own SQLite table and atomic revision checks. Missing policy
is explicitly unconfigured; corrupt storage/auth outages fail with errors.
No policy, PIN or hash is copied from self-editable userdata. The strict new
schema does not accept PIN material. Ordinary userdata retains its legacy
pass-through behavior and may still contain old client PIN hashes. New policy
responses are uncached and do not expose bearer tokens, PIN material or the
stored administrator audit ID.

This producer alone does not protect media routes. Trusted administrative
migration, classification provenance, consumer route enforcement and profile/PIN
grant design remain required before claiming FR-PLAY-007 complete.

## Transport

- **gRPC listener** (`USERDATA_LOCAL_GRPC_ADDR`, default `:9703`): TLS enabled by default via `internal/grpctls` (auto-generated dev certs under `~/.muxcore/tls/userdata-local` or `MUXCORE_TLS_*` / `USERDATA_TLS_*`). Set `MUXCORE_INSECURE_DISABLE_TLS=true` for localhost plaintext dev only.
- **Outbound auth-local dial**: uses the same mesh client TLS material (`MUXCORE_TLS_CERT`, `MUXCORE_TLS_KEY`, `MUXCORE_TLS_CA`) unless insecure dev mode is enabled for localhost.
- **HTTP** (`USERDATA_LOCAL_HTTP_ADDR`): serve behind a trusted reverse proxy or bind to localhost; session validation is the access control boundary.

## Configuration

| Variable | Purpose |
|----------|---------|
| `AUTH_LOCAL_GRPC_ADDR` | auth-local gRPC dial target (default `localhost:9403`) |
| `MUXCORE_TLS_*` / `USERDATA_TLS_*` | Listener and client TLS material |
| `MUXCORE_INSECURE_DISABLE_TLS` | Dev-only plaintext gRPC (do not use in production) |

Fixes umbrella [#32](https://github.com/Muxcore-Media/umbrella/issues/32).

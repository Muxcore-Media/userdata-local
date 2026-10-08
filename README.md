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
| `USERDATA_LOCAL_DATA_DIR` | `$MUXCORE_DATA_DIR/userdata`, else `./data/userdata` | Module data dir (inside backup coverage, ADR-0013) |
| `USERDATA_LOCAL_DB_PATH` | `<data dir>/userdata.db` | SQLite database file (overrides the data-dir default) |
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
| `GET` | `/api/parental-policy` | Read authoritative policy for self or a same-tenant account as admin |
| `PUT` | `/api/parental-policy` | Admin-only, revision-checked policy replacement |
| `GET` | `/health` | Unauthenticated health check |

### Blob sections (in scope)

| Key | Purpose |
|-----|---------|
| `progress` | Resume position, watched flag, continue-watching source |
| `favorites` | Starred titles |
| `prefs` | Display/home/playback preferences (stored for sync), plus pass-through `parental` |
| `playlists`, `queue` | Pass-through sections for client sync |

Progress merge uses `updatedAt` (newer wins). Watched auto-detection uses the 92% threshold from native clients.

### Authoritative parental policy (provider foundation)

The separate `userdata.parental-policy.v1` resource implements the producer contract
in umbrella ADR-0030. Existing BFF/admin flows do not consume it yet, and this
addition does **not** enforce parental restrictions on media. FR-PLAY-007 remains
partial. Existing userdata preferences, including pass-through `prefs.parental`,
remain untrusted client state and never initialize or overwrite this resource.

The public `parental` package also hosts the shared, pure evaluator for these
semantics (`parental.Evaluate`, `RatingLevel`; umbrella ADR-0031 Decision 2.6).
It is unused until the BFF gate consumes it and performs no I/O.

Both methods require `Authorization: Bearer <current-session>` and
`X-MuxCore-User-Id: <target-account>`. Only this bearer is accepted: cookies,
`x-auth-token`, module certificates and identity/tenant headers cannot substitute
for it. Each request revalidates current roles and tenant with auth-local.
Users can read their own policy. Admins can read/write their own or another
existing account in the same verified tenant; managers cannot write policy.
Another account is resolved through auth-local `ListUsers`. Empty tenant is the
single-household scope, not a wildcard. Query selectors are unsupported.

GET returns trusted `user_id` and `tenant_id` with one of these states:

```json
{"user_id":"kid","tenant_id":"home","state":"unconfigured","revision":0,"policy":null}
```

```json
{"user_id":"kid","tenant_id":"home","state":"configured","revision":1,"policy":{"version":1,"mode":"unrestricted","rules":null},"updated_at":"2026-10-07T00:00:00Z"}
```

Unconfigured, unavailable and explicit unrestricted are distinct. Future
consumers must not interpret missing records, unsupported capability, invalid
data or failed requests as unrestricted access.

PUT accepts a complete replacement and its expected revision (zero to create):

```json
{
  "expected_revision": 0,
  "policy": {
    "version": 1,
    "mode": "restricted",
    "rules": {
      "kids_mode": true,
      "max_rating": "PG",
      "blocked_tags": ["horror"],
      "allowed_tags": [],
      "allow_unrated": false
    }
  }
}
```

All illustrated fields are required; unknown/duplicate fields, null rule fields,
trailing JSON, unsupported versions/ratings, and PIN/PIN-hash fields are rejected.
Unrestricted mode requires `rules:null`. There is no DELETE; disabling a policy
requires an explicit unrestricted replacement at its current revision.

Tag arrays allow up to 64 entries of 128 bytes each. Entries are trimmed,
lowercased, sorted and deduplicated; empty/control-containing tags are rejected.
Future enforcement must match complete normalized tags: blocked tags win, and a
nonempty allowed set requires a match without overriding rating/unrated rules.
Kids mode supplies a PG ceiling when `max_rating` is empty. Supported ceilings
are `G`, `TV-Y`, `TV-Y7`, `TV-Y7-FV`, `ALL`, `E`, `PG`, `TV-G`, `TV-PG`, `E10+`,
`PG-13`, `TV-14`, `T`, `R`, `TV-MA`, `M`, `MA`, `NC-17`, `AO`, and `X`; case and
surrounding whitespace normalize. Unknown/malformed media classifications remain
unavailable to future consumers, distinct from genuinely unrated content.

Responses use `Cache-Control: no-store`. Errors return a stable `code` and no
submitted policy/credentials: 400 invalid input, 401 unauthenticated, 403
forbidden, 404 unknown target account, 409 stale revision, 413 body over 32 KiB,
405 unsupported method, or 503 auth/storage unavailable. Successful PUT returns
the configured document with an incremented revision. Updates use SQLite
compare-and-swap; callers must read/review current policy before retrying a 409.

Policies persist in an additive `parental_policies` table keyed by
`(tenant_id,user_id)`, independently of `user_blobs`. Database backups include
the table. The older binary can still read ordinary userdata while leaving the
new table intact; this is a storage rollback property, not a guarantee of media
enforcement after consumer integration. No legacy policy is automatically
migrated. Follow-up migration must read the protected admin-ui `parental.json`
and use authenticated revision-checked writes. Profile/PIN grants and complete
BFF media enforcement remain separate work.

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
| `userdata.parental-policy.v1` | Independent authoritative account policy resource (HTTP; currently unused by consumers) |
| `settings` | Module settings surface (`db_path`) |

---

## Tests

```bash
go test ./...
```

Offline SQLite/fixture tests only (mock auth provider; no live auth-local required).

## License

GPL-3.0

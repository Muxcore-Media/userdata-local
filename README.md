# Userdata Local

MuxCore household userdata sidecar module.

Durable per-user library state for the household loop: watch progress / resume position, continue-watching rows, favorites, and client preferences. Persists data in local SQLite; exposes gRPC for mesh consumers and HTTP compatible with `/api/userdata` clients.

Tracked by https://github.com/Muxcore-Media/umbrella/issues/19 · Security: https://github.com/Muxcore-Media/umbrella/issues/32

---

## How it works

```
media-ui / admin-ui ──HTTPS (mTLS + Bearer + user header)──► userdata-local (:9701) ──► SQLite
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
| `MUXCORE_TLS_DIR` | — | Existing module identity directory (`module.crt`, `module.key`, `ca.crt`) for separately invoked helpers |
| `MUXCORE_CA_EXPORT_DIR` | — | Mounted core CA directory (`ca.crt`), when `MUXCORE_TLS_CA` is unset |
| `MUXCORE_PROFILE` | `household` unless an insecure flag infers legacy dev | `dev`, `household`, or `staging` (household alias); unknown values fail |
| `USERDATA_TLS_CERT` / `USERDATA_TLS_KEY` / `USERDATA_TLS_CA` | — | gRPC listener TLS overrides |
| `MUXCORE_INSECURE_DISABLE_TLS` | — | Explicit dev-only plaintext (`true` or `1`); HTTP also checks legacy `MUXCORE_GRPC_INSECURE` and `MUXCORE_DEV_TLS_SKIP` aliases |

---

## Authentication

### HTTP

Household/staging HTTP is TLS-only with a core-CA-verified client certificate.
Both the listener and client independently enforce the profile. Missing, partial,
malformed, expired or wrong-identity material fails configuration; neither side
generates an HTTP CA, uses system roots or falls back to plaintext. TLS 1.2 is the
minimum. The provider certificate must have CN and service SAN `userdata-local`.
Every client verifies that fixed SAN and exact CN even when dialing loopback;
`MUXCORE_TLS_SERVER_NAME` does not override the provider identity.
The provider's HTTP certificate identity stays `userdata-local` even if the
historical SDK `MUXCORE_MODULE_ID` override names a different registration ID.

Admission uses only the verified client certificate CN, before any HTTP handler,
user lookup or storage access:

| Module CN | `/api/parental-policy` | `/api/userdata` | `/health` |
|-----------|------------------------|-----------------|-----------|
| `media-ui` | GET | GET, PUT | GET, HEAD |
| `admin-ui` | GET, PUT | GET, PUT | GET, HEAD |
| `userdata-local`, `health-monitor` | denied | denied | GET, HEAD |
| All others (including `jellyfin`, `muxcore`) | denied | denied | denied |

Untrusted/missing certificates fail the handshake. Admission denials return
no-store 403 with exactly `{"code":"userdata.module_forbidden"}` and
`X-MuxCore-Error-Code: userdata.module_forbidden`. HEAD has no body; that generic
header preserves its discriminator. Unknown paths/methods are denied before mux
redirects. Module identity never comes from HTTP headers or the user bearer.

Admitted data requests must still send **both**:

1. `Authorization: Bearer <session-token>` — validated via auth-local
2. `X-MuxCore-User-Id: <household-user-id>` — must match the validated session (unless caller has `admin` role)

Requests with a spoofed user id or missing/invalid bearer token receive `401` / `403`.
Certificate identity never substitutes for user authority. A `media-ui` caller
cannot PUT policy even with an admin bearer; `admin-ui` still needs a fresh admin
bearer and the verified tenant/target checks. Browsers/native devices access a
user-facing gateway; do not give them mesh private keys. Optional Jellyfin
background userdata sync has no admitted HTTP operation in this release.

Explicit insecure dev retains plaintext and user authorization, logs a warning,
and has no authenticated module transport. Dev without an insecure flag still
requires TLS. Household/staging reject all three insecure aliases, including `1`.
With neither a profile nor an insecure flag, the HTTP boundary infers secure
household operation and fails if identity material is absent.

### Checked provider client

The public `github.com/Muxcore-Media/userdata-local/httpclient` package owns
provider transport. Construct it after the calling daemon has enrolled, or resolve
existing mounted identity files with `FromEnv` in a separate helper. Pass the
calling service's fixed module ID; a certificate with a different CN is rejected.

```go
cfg, err := httpclient.FromEnv("https://userdata-local:9672", "media-ui")
// Handle configuration errors; never retry using plaintext.
client, err := httpclient.New(cfg)
// Reuse client, and call CloseIdleConnections on shutdown/reconfiguration.
headers := http.Header{
    "Authorization": {"Bearer " + currentSessionBearer},
    "X-Muxcore-User-Id": {verifiedTargetUser},
}
response, err := client.Do(ctx, httpclient.GetPolicy, headers, nil)
```

`Do` supports only `GetPolicy`, `PutPolicy`, `GetUserdata`, `PutUserdata`,
`GetHealth` and `HeadHealth`; each operation fixes the method and path. Callers
derive the one bearer/target from their current server session; the package copies
headers and never creates authorization. The configured origin has no path,
credentials, query or fragment. HTTPS is mandatory in secure mode; explicit
insecure dev uses HTTP. Unspecified bind addresses are not valid origins.
The dedicated transport refuses different origins before dialing, ignores proxy
environment variables and refuses every redirect, including same-origin ones.

Requests and response reads default to 5 seconds (configurable up to 30 seconds),
honor earlier cancellation and bound response bodies to 8 MiB and headers to
32 KiB. Callers close returned response bodies. Application statuses remain intact,
including policy `401`, `403 policy.forbidden`, `409` and `503`. Transport or
admission failures satisfy `errors.Is(err, httpclient.ErrUnavailable)`; use
`errors.As` for `*httpclient.UnavailableError` and its `Reason`. Only
`ReasonModuleForbidden` proves the write was not applied. Connection loss/timeouts
leave write outcome uncertain. Do not revoke a user's session for module denial,
interpret a failed policy fetch as unrestricted, or blindly retry an uncertain
write with a new revision.

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
| `GET`, `HEAD` | `/health` | Admitted mesh health check; no user bearer, empty HEAD body |

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

Example authenticated HTTP request for **explicit insecure dev only**:

```bash
curl -H "Authorization: Bearer $SESSION" \
     -H "X-MuxCore-User-Id: alice" \
     http://localhost:9701/api/userdata
```

`make build` also produces `./userdata-health`; the Docker image ships it at
`/app/userdata-health`. Run it inside userdata-local's own service context:

```bash
./userdata-health
# Or explicitly set the origin (for example the household compose port):
./userdata-health --origin https://127.0.0.1:9672
```

The default probe uses `USERDATA_LOCAL_HTTP_ADDR` (default `:9701`), translating a
local wildcard bind to loopback. It validates the same provider identity, calls
GET `/health` without a user bearer, and exits nonzero on failure. It never starts
the daemon, opens its database, enrolls, consumes a bootstrap token or creates
certificates. Since daemon-exported environment values are not inherited by a
separate process, configure explicit cert/key/CA paths or `MUXCORE_TLS_DIR` plus
the mounted CA. CA lookup is explicit `MUXCORE_TLS_CA`, then
`MUXCORE_CA_EXPORT_DIR/ca.crt`, then `MUXCORE_TLS_DIR/ca.crt`. Partial explicit
cert/key paths fail instead of falling back to the directory.

Deploy the listener together with compatible BFF/admin clients and authenticated
probes. Old household plaintext callers deliberately fail. No parallel plaintext
listener or automatic authority migration is provided. Provider tests alone do
not establish BFF cache propagation, native-client routing, household enforcement
or deployment acceptance (umbrella ADR-0033/S9).

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

# Compatibility

- **MuxCore core:** `v0.5.8+`
- **Go:** `1.26.4+`
- **Client blob schema:** aligned with `muxcore-ios` / `media-ui-app` userdata JSON
- **Capability:** `userdata.local` (no legacy alias yet)

`contracts-media` does not yet define userdata events; this module owns blob schema until shared contracts land in core.

Unreleased ADR-0038 adds HTTP-only `userdata.conditional.v2` for verified
household self access through `media-ui`. Supported families are preferences,
favorites, want-to-watch, playlists and queue; progress/watched is unsupported.
The public `conditional` API exposes raw JSON/string types. New fixed client
operations retain transport checks. Legacy HTTP/gRPC/fallback APIs, dependencies,
version `0.1.8` and DDL remain unchanged.

Every Store reopen rotates an unpersisted authority epoch. Restore/reset requires
shutdown/reopen and one active provider/Store per database. Old tokens are refused
but unresolved requests remain uncertain. Request IDs correlate only; each valid
request advances once, even a data no-op. GET and retry conflict cannot acknowledge
a lost reply. README defines strict JSON, bounds, scope and error codes.

Legacy writers can undo removals and discard unknown data. Conditional support
does not mean writer enforcement or device durability. Consumers require a
published producer version and separate reviewed migration before activation;
no automatic legacy downgrade is valid after failure or rollback. Unchanged DDL
requires no new upgrade snapshot; existing fixtures remain full-suite gates.

Unreleased ADR-0033 HTTP transport requires coordinated consumer deployment:
household/staging listeners reject plaintext and unadmitted module identities.
Consumers must use the public `httpclient` package with their own enrolled
identity and retain current user-bearer authorization. Explicit insecure dev
remains supported with a warning. The existing gRPC boundary and database schema
are unchanged; policy revisions and blobs persist across the transport switch.
The provider image and host build include a separate `userdata-health` executable
for authenticated local readiness. This source does not supply consumer/deploy
integration or establish full S9 acceptance.

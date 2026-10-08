# Compatibility

- **MuxCore core:** `v0.5.8+`
- **Go:** `1.26.4+`
- **Client blob schema:** aligned with `muxcore-ios` / `media-ui-app` userdata JSON
- **Capability:** `userdata.local` (no legacy alias yet)

`contracts-media` does not yet define userdata events; this module owns blob schema until shared contracts land in core.

Unreleased ADR-0033 HTTP transport requires coordinated consumer deployment:
household/staging listeners reject plaintext and unadmitted module identities.
Consumers must use the public `httpclient` package with their own enrolled
identity and retain current user-bearer authorization. Explicit insecure dev
remains supported with a warning. The existing gRPC boundary and database schema
are unchanged; policy revisions and blobs persist across the transport switch.
The provider image and host build include a separate `userdata-health` executable
for authenticated local readiness. This source does not supply consumer/deploy
integration or establish full S9 acceptance.

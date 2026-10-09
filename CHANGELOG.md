# Changelog

## [Unreleased]

### Added
- ADR-0035/T-M4-07 slice E3: the shared `erasure.Reconciler` (core `sdk/go/module/erasure`
  v0.6.17, sdk/go/module v0.6.7) applies the identity provider's erasure ledger. One transaction
  deletes the user's `user_blobs` and `parental_policies` rows, anonymises `updated_by` on other
  users' policies to `deleted-user`, and records the new `erasure_applied` table. Configured by
  `ERASURE_SWEEP_INTERVAL` (default 5m); the household profile requires a core connection.
- Writes for a user id with an applied erasure are refused: HTTP `410`
  (`userdata.account_erased`, `policy.account_erased`), gRPC `Put` `FailedPrecondition`.
- ADR-0015 upgrade snapshot `v0.1.6.db`.

### Changed
- Dependencies: core v0.6.17, sdk/go/module v0.6.7.

## [0.1.6] - 2026-10-09

Source release only (ADR-0033): the HTTP provider is mTLS-only in the household profile, so BFF and admin-ui must consume the new `httpclient` and the deployment must switch to `https://` before any running household takes this version (consumers S9b/S9c, deployment S9d).

### Added
- ADR-0033/S9a HTTP transport: strict core-CA mTLS, fixed provider SAN/CN and
  verified caller-CN method/path admission before existing user authorization.
- Public `httpclient` package with fixed operations, caller identity validation,
  origin binding, no proxy/redirect behavior, bounded requests (one deadline for
  connect through complete body read, rechecked after the read) and typed provider
  unavailability distinct from application authorization failures.
- Standalone `userdata-health` probe in host/image packaging, resolving only
  existing configured or mounted identity files without enrollment or storage.

### Changed
- HTTP insecure-flag detection now matches core and the SDK exactly: only
  `MUXCORE_INSECURE_DISABLE_TLS` / `MUXCORE_DEV_TLS_SKIP` equal to `true` or `1`
  select plaintext. `MUXCORE_GRPC_INSECURE` and other spellings (`TRUE`, `yes`)
  no longer select plaintext HTTP, and invalid values are ignored rather than
  rejected, so an unset profile with such a value resolves to secure household
  with module admission instead of an unauthenticated listener.
- `userdata-health` / `httptransport.FromEnv` resolve identity like the SDK's
  `meshid.Ensure`: default identity directory `$MUXCORE_DATA_DIR/mesh-id` (data
  dir default `./data`), and `MUXCORE_CA_EXPORT_DIR/ca.crt` only when it exists.
- A `MUXCORE_MODULE_ID` override other than `userdata-local` is unsupported for
  secure HTTP and now fails startup/`FromEnv` with a clear message (certificate CN
  mismatches also name both IDs).
- `PUT /api/userdata` (and gRPC `Put`) refuses a merged blob over 8 MiB with `413`
  (`ResourceExhausted`) instead of storing a blob the checked client cannot read;
  `httpclient.MaxResponseBytes` is tied to the same limit.
- Household HTTP intentionally rejects legacy plaintext clients; compatible
  consumers and authenticated probes must deploy together. Explicit insecure dev
  remains available with profile validation and warnings. No DB schema or gRPC
  principal model change; no full S9/deployment acceptance is implied.

## [0.1.5] - 2026-10-08

### Added
- Authoritative parental policy resource `GET`/`PUT /api/parental-policy` (capability `userdata.parental-policy.v1`, ADR-0030): bearer-authenticated, revision-checked, stored apart from user blobs. Not yet consumed by the BFF or admin-ui.
- `parental.Evaluate` / `RatingLevel`: shared pure parental-policy evaluator (ADR-0031 S1). Not yet consumed by any module.

## [0.1.4] - 2026-10-05


### Changed
- Default database moved from `$HOME/.muxcore/userdata.db` to `<data dir>/userdata.db` (`USERDATA_LOCAL_DATA_DIR`, else `$MUXCORE_DATA_DIR/userdata`, else `./data/userdata`) so household progress/favorites fall under backups (ADR-0013). `USERDATA_LOCAL_DB_PATH` still overrides. If only the legacy file exists, startup logs a warning with the exact `mv` command; data is never moved automatically.

## [0.1.3] - 2026-10-05

### Changed
- Built on core v0.6.14 / sdk/go/module v0.6.4: unregisters on shutdown and re-registers after core restarts (ADR-0022).

## [0.1.2] - 2026-10-05


### Changed
- Reported version comes from muxcore.json (ADR-0021); built on core v0.6.12 / sdk/go/module v0.6.3 (mesh enrollment, ADR-0017).

## [0.1.1] - 2026-10-05


### Added
- Upgrade test (`internal/store/upgrade_test.go`) opening the committed v0.1.0 snapshot (`internal/store/testdata/upgrade/`) with the current code (ADR-0015, NFR-DATA-002, FR-INS-005).

### Changed
- Test dependency `core/sdk/go/module` v0.6.1 (moduletest); `modernc.org/sqlite` resolved to v1.55.0 transitively.

## [0.1.0] - 2026-10-05

### Changed
- CI runs on GitHub-hosted runners from the umbrella template; retired-origin workflows removed.
- Dependencies resolve from published GitHub tags (no filesystem `replace`); requires core v0.6.0.

- Preserve `prefs.parental` through Put/Get so admin-ui snake_case kids/max rating/PIN survive (umbrella#89 residual A)

## 0.1.0 — 2026-09-05

- Initial MuxCore sidecar implementation
- SQLite persistence for per-user blobs
- gRPC `UserDataService` and HTTP `/api/userdata`
- Continue-watching, favorites, watched/progress merge logic
- Fixture tests (`go test ./...`)

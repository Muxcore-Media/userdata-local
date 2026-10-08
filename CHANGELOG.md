# Changelog

## [Unreleased]

### Added
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

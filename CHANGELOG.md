# Changelog

## Unreleased

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

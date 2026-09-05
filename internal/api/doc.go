// Package api documents the userdata-local internal persistence contract.
//
// Userdata is stored as one JSON blob per user_id with sections:
//   - progress: map[item_id]ProgressEntry (resume + watched)
//   - favorites: map[item_id]FavoriteEntry
//   - prefs, playlists, queue: client sync pass-through
//
// gRPC entry points live in internal/server; SQLite in internal/store.
// JSON shapes are defined in internal/models and mirror muxcore-ios UserDataModels.swift.
package api

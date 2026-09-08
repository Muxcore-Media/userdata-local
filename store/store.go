// Package store is the public file-backed userdata API used by mediauiprox
// and media-library-maintainer as a local fallback when the mesh sidecar is
// offline. The sidecar itself persists in SQLite under internal/store.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Scope identifies a household userdata document.
type Scope struct {
	TenantID string `json:"tenant_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
}

// Blob is the wire document exchanged with media-ui-app. Nested maps stay
// json.RawMessage so partial client payloads merge without a schema lock.
type Blob struct {
	Progress  map[string]json.RawMessage `json:"progress,omitempty"`
	Favorites map[string]json.RawMessage `json:"favorites,omitempty"`
	Prefs     json.RawMessage            `json:"prefs,omitempty"`
	Playlists json.RawMessage            `json:"playlists,omitempty"`
	Queue     json.RawMessage            `json:"queue,omitempty"`
	UserID    string                     `json:"user_id,omitempty"`
	TenantID  string                     `json:"tenant_id,omitempty"`
}

// Store persists one JSON file per scope under dir.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New opens (or creates) a directory-backed userdata store.
func New(dir string) (*Store, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "muxcore-media-userdata")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func sanitize(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "anonymous"
	}
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(id)
}

func (s *Store) path(scope Scope) string {
	user := sanitize(scope.UserID)
	if scope.TenantID != "" {
		return filepath.Join(s.dir, "tenants", sanitize(scope.TenantID), user+".json")
	}
	return filepath.Join(s.dir, user+".json")
}

// Get returns the stored blob, or an empty blob if none exists.
func (s *Store) Get(scope Scope) Blob {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path(scope))
	if err != nil {
		return Blob{}
	}
	var blob Blob
	if json.Unmarshal(raw, &blob) != nil {
		return Blob{}
	}
	return blob
}

// Put merges incoming into the stored blob and writes the result.
func (s *Store) Put(scope Scope, incoming Blob) (Blob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := Blob{}
	if raw, err := os.ReadFile(s.path(scope)); err == nil {
		_ = json.Unmarshal(raw, &current)
	}
	merged := mergeBlob(current, incoming)
	if scope.UserID != "" {
		merged.UserID = scope.UserID
	}
	if scope.TenantID != "" {
		merged.TenantID = scope.TenantID
	}
	path := s.path(scope)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Blob{}, err
	}
	body, err := json.Marshal(merged)
	if err != nil {
		return Blob{}, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return Blob{}, err
	}
	return merged, nil
}

func mergeBlob(stored, incoming Blob) Blob {
	out := stored
	if incoming.Progress != nil {
		out.Progress = mergeByUpdatedAt(out.Progress, incoming.Progress)
	}
	if incoming.Favorites != nil {
		if out.Favorites == nil {
			out.Favorites = map[string]json.RawMessage{}
		}
		for id, entry := range incoming.Favorites {
			out.Favorites[id] = entry
		}
	}
	if len(incoming.Prefs) > 0 {
		out.Prefs = incoming.Prefs
	}
	if len(incoming.Playlists) > 0 {
		out.Playlists = incoming.Playlists
	}
	if len(incoming.Queue) > 0 {
		out.Queue = incoming.Queue
	}
	return out
}

func mergeByUpdatedAt(local, incoming map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(local)+len(incoming))
	for id, entry := range local {
		out[id] = entry
	}
	for id, entry := range incoming {
		cur, ok := out[id]
		if !ok || updatedAt(entry) >= updatedAt(cur) {
			out[id] = entry
		}
	}
	return out
}

func updatedAt(raw json.RawMessage) string {
	var row struct {
		UpdatedAt string `json:"updatedAt"`
	}
	if json.Unmarshal(raw, &row) != nil {
		return ""
	}
	return row.UpdatedAt
}

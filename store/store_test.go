package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPutMergesFresherProgress(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{UserID: "alice"}
	first, _ := json.Marshal(map[string]any{"id": "m1", "positionSec": 10, "updatedAt": "2026-01-01T00:00:00Z"})
	older, _ := json.Marshal(map[string]any{"id": "m1", "positionSec": 1, "updatedAt": "2025-01-01T00:00:00Z"})
	if _, err := st.Put(scope, Blob{Progress: map[string]json.RawMessage{"m1": first}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Put(scope, Blob{Progress: map[string]json.RawMessage{"m1": older}})
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		PositionSec int `json:"positionSec"`
	}
	if err := json.Unmarshal(got.Progress["m1"], &p); err != nil {
		t.Fatal(err)
	}
	if p.PositionSec != 10 {
		t.Fatalf("want 10, got %d", p.PositionSec)
	}
}

func TestTenantScopedPath(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{TenantID: "acme", UserID: "alice"}
	raw, _ := json.Marshal(map[string]any{"id": "m1", "updatedAt": "2026-01-01T00:00:00Z"})
	if _, err := st.Put(scope, Blob{Progress: map[string]json.RawMessage{"m1": raw}}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "tenants", "acme", "alice.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected %s: %v", want, err)
	}
}

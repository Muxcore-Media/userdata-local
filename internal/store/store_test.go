package store_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "userdata.db")
	st, err := store.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPutGetProgressAndContinueWatching(t *testing.T) {
	st := openTestStore(t)
	userID := "user-1"
	watched := false
	entry := models.ProgressEntry{
		ID:          "movie-1",
		Kind:        models.MediaKindMovie,
		Title:       "Example",
		Href:        "/movies/movie-1",
		PositionSec: 120,
		DurationSec: 3600,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		Watched:     &watched,
	}
	if _, _, err := st.UpsertProgress(userID, entry); err != nil {
		t.Fatalf("UpsertProgress: %v", err)
	}
	items, err := st.ListContinueWatching(userID, 10)
	if err != nil {
		t.Fatalf("ListContinueWatching: %v", err)
	}
	if len(items) != 1 || items[0].ID != "movie-1" {
		t.Fatalf("continue=%+v", items)
	}
	blob, rev, err := st.Get(userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rev != 1 {
		t.Fatalf("revision=%d", rev)
	}
	if blob.Progress["movie-1"].PositionSec != 120 {
		t.Fatalf("progress=%+v", blob.Progress["movie-1"])
	}
}

func TestNormalizeProgressMarksWatched(t *testing.T) {
	entry := models.ProgressEntry{
		ID:          "movie-2",
		Kind:        models.MediaKindMovie,
		Title:       "Done",
		Href:        "/movies/movie-2",
		PositionSec: 920,
		DurationSec: 1000,
	}
	normalized := store.NormalizeProgress(entry)
	if normalized.Watched == nil || !*normalized.Watched {
		t.Fatalf("expected watched=true, got %+v", normalized.Watched)
	}
	if normalized.PositionSec != 0 {
		t.Fatalf("position=%v", normalized.PositionSec)
	}
}

func TestMergeProgressUsesUpdatedAt(t *testing.T) {
	local := map[string]models.ProgressEntry{
		"a": {ID: "a", UpdatedAt: "2026-01-02T00:00:00Z", PositionSec: 10},
	}
	incoming := map[string]models.ProgressEntry{
		"a": {ID: "a", UpdatedAt: "2026-01-01T00:00:00Z", PositionSec: 99},
		"b": {ID: "b", UpdatedAt: "2026-01-03T00:00:00Z", PositionSec: 5},
	}
	merged := store.MergeProgress(local, incoming)
	if merged["a"].PositionSec != 10 {
		t.Fatalf("local newer entry overwritten: %+v", merged["a"])
	}
	if merged["b"].PositionSec != 5 {
		t.Fatalf("missing incoming: %+v", merged["b"])
	}
}

func TestToggleFavorite(t *testing.T) {
	st := openTestStore(t)
	userID := "user-2"
	entry := models.FavoriteEntry{
		ID:    "movie-9",
		Kind:  models.MediaKindMovie,
		Title: "Fav",
		Href:  "/movies/movie-9",
	}
	_, _, added, err := st.ToggleFavorite(userID, entry)
	if err != nil || !added {
		t.Fatalf("add favorite: added=%v err=%v", added, err)
	}
	blob, _, err := st.Get(userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := blob.Favorites["movie-9"]; !ok {
		t.Fatal("favorite missing")
	}
	_, _, added, err = st.ToggleFavorite(userID, entry)
	if err != nil || added {
		t.Fatalf("remove favorite: added=%v err=%v", added, err)
	}
	blob, _, err = st.Get(userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := blob.Favorites["movie-9"]; ok {
		t.Fatal("favorite should be removed")
	}
}

func TestMarkWatched(t *testing.T) {
	st := openTestStore(t)
	userID := "user-3"
	watched := false
	entry := models.ProgressEntry{
		ID:          "tv-1",
		Kind:        models.MediaKindTV,
		Title:       "Show",
		Href:        "/tv/tv-1",
		PositionSec: 500,
		DurationSec: 1800,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		Watched:     &watched,
	}
	if _, _, err := st.UpsertProgress(userID, entry); err != nil {
		t.Fatalf("UpsertProgress: %v", err)
	}
	if _, _, err := st.MarkWatched(userID, "tv-1", true); err != nil {
		t.Fatalf("MarkWatched: %v", err)
	}
	blob, _, err := st.Get(userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := blob.Progress["tv-1"]
	if got.Watched == nil || !*got.Watched || got.PositionSec != 0 {
		t.Fatalf("progress=%+v", got)
	}
	items, err := st.ListContinueWatching(userID, 10)
	if err != nil {
		t.Fatalf("ListContinueWatching: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("watched item in continue list: %+v", items)
	}
}

func TestPutGetPreservesSnakeCaseParental(t *testing.T) {
	st := openTestStore(t)
	incoming, err := store.ParseBlob([]byte(`{
		"prefs": {
			"parental": {
				"kids_mode": true,
				"max_parental_rating": "PG",
				"pin_hash": "abc123def456"
			}
		}
	}`))
	if err != nil {
		t.Fatalf("ParseBlob: %v", err)
	}
	if _, _, err := st.Put("user-parental", incoming); err != nil {
		t.Fatalf("Put: %v", err)
	}
	blob, _, err := st.Get("user-parental")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if blob.Prefs == nil || len(blob.Prefs.Parental) == 0 {
		t.Fatalf("parental missing after Get: %+v", blob.Prefs)
	}
	var parental map[string]any
	if err := json.Unmarshal(blob.Prefs.Parental, &parental); err != nil {
		t.Fatalf("decode parental: %v", err)
	}
	if parental["kids_mode"] != true {
		t.Fatalf("kids_mode=%v", parental["kids_mode"])
	}
	if parental["max_parental_rating"] != "PG" {
		t.Fatalf("max_parental_rating=%v", parental["max_parental_rating"])
	}
	if parental["pin_hash"] != "abc123def456" {
		t.Fatalf("pin_hash=%v", parental["pin_hash"])
	}
}

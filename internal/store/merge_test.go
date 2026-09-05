package store_test

import (
	"testing"

	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/internal/store"
)

func TestContinueWatchingFilters(t *testing.T) {
	watched := true
	unwatched := false
	progress := map[string]models.ProgressEntry{
		"watched": {
			ID: "watched", UpdatedAt: "2026-01-03T00:00:00Z", PositionSec: 100, DurationSec: 1000,
			Watched: &watched,
		},
		"too-early": {
			ID: "too-early", UpdatedAt: "2026-01-02T00:00:00Z", PositionSec: 2, DurationSec: 1000,
			Watched: &unwatched,
		},
		"almost-done": {
			ID: "almost-done", UpdatedAt: "2026-01-01T00:00:00Z", PositionSec: 930, DurationSec: 1000,
			Watched: &unwatched,
		},
		"in-progress": {
			ID: "in-progress", UpdatedAt: "2026-01-04T00:00:00Z", PositionSec: 300, DurationSec: 1000,
			Watched: &unwatched,
		},
	}
	items := store.ContinueWatching(progress, 10)
	if len(items) != 1 || items[0].ID != "in-progress" {
		t.Fatalf("items=%+v", items)
	}
}

func TestParseBlobDefaults(t *testing.T) {
	blob, err := store.ParseBlob([]byte(`{"progress":{"a":{"id":"a","kind":"movie","title":"A","href":"/a","positionSec":1,"durationSec":10,"updatedAt":"2026-01-01T00:00:00Z"}}}`))
	if err != nil {
		t.Fatalf("ParseBlob: %v", err)
	}
	if blob.Favorites == nil || blob.Prefs == nil {
		t.Fatalf("blob=%+v", blob)
	}
}

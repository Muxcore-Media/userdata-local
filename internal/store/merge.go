package store

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/Muxcore-Media/userdata-local/internal/models"
)

const watchedThreshold = 0.92

// NormalizeProgress applies watched/position rules matching muxcore-ios UserDataStore.upsertProgress.
func NormalizeProgress(entry models.ProgressEntry) models.ProgressEntry {
	next := entry
	if next.UpdatedAt == "" {
		next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if entry.Watched == nil {
		ratio := 0.0
		if next.DurationSec > 0 {
			ratio = next.PositionSec / next.DurationSec
		}
		if ratio >= watchedThreshold {
			watched := true
			next.Watched = &watched
			next.PositionSec = 0
		}
	} else if entry.Watched != nil && *entry.Watched {
		next.PositionSec = 0
	}
	return next
}

// MergeProgress merges server progress into local using updatedAt (client-compatible).
func MergeProgress(local, incoming map[string]models.ProgressEntry) map[string]models.ProgressEntry {
	out := make(map[string]models.ProgressEntry, len(local)+len(incoming))
	for id, entry := range local {
		out[id] = entry
	}
	for id, entry := range incoming {
		cur, ok := out[id]
		if !ok || entry.UpdatedAt >= cur.UpdatedAt {
			out[id] = entry
		}
	}
	return out
}

// MergeBlob merges an incoming partial blob into the stored blob.
func MergeBlob(stored, incoming models.Blob) models.Blob {
	out := stored
	if out.Progress == nil {
		out.Progress = map[string]models.ProgressEntry{}
	}
	if out.Favorites == nil {
		out.Favorites = map[string]models.FavoriteEntry{}
	}
	if incoming.Progress != nil {
		out.Progress = MergeProgress(out.Progress, incoming.Progress)
	}
	if incoming.Favorites != nil {
		for id, entry := range incoming.Favorites {
			out.Favorites[id] = entry
		}
	}
	if incoming.Prefs != nil {
		out.Prefs = incoming.Prefs
	}
	if incoming.Playlists != nil {
		out.Playlists = incoming.Playlists
	}
	if incoming.Queue != nil {
		out.Queue = incoming.Queue
	}
	return out
}

// ContinueWatching returns in-progress items sorted by updatedAt desc.
func ContinueWatching(progress map[string]models.ProgressEntry, limit int) []models.ProgressEntry {
	if limit <= 0 {
		limit = 24
	}
	var items []models.ProgressEntry
	for _, entry := range progress {
		if entry.Watched != nil && *entry.Watched {
			continue
		}
		if entry.PositionSec <= 5 {
			continue
		}
		if entry.DurationSec > 0 && entry.PositionSec/entry.DurationSec >= watchedThreshold {
			continue
		}
		items = append(items, entry)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].UpdatedAt > items[j].UpdatedAt
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

// ParseBlob decodes JSON into a Blob, filling missing maps.
func ParseBlob(raw []byte) (models.Blob, error) {
	if len(raw) == 0 {
		return models.EmptyBlob(), nil
	}
	var blob models.Blob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return models.Blob{}, err
	}
	if blob.Progress == nil {
		blob.Progress = map[string]models.ProgressEntry{}
	}
	if blob.Favorites == nil {
		blob.Favorites = map[string]models.FavoriteEntry{}
	}
	if blob.Prefs == nil {
		prefs := models.DefaultPreferences()
		blob.Prefs = &prefs
	}
	if blob.Playlists == nil {
		blob.Playlists = []models.Playlist{}
	}
	if blob.Queue == nil {
		blob.Queue = []models.QueueItem{}
	}
	return blob, nil
}

// MarshalBlob encodes a blob as JSON.
func MarshalBlob(blob models.Blob) ([]byte, error) {
	return json.Marshal(blob)
}

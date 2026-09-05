package models

// MediaKind identifies the library item type stored in userdata blobs.
type MediaKind string

const (
	MediaKindMovie   MediaKind = "movie"
	MediaKindTV      MediaKind = "tv"
	MediaKindEpisode MediaKind = "episode"
	MediaKindMusic   MediaKind = "music"
	MediaKindBook    MediaKind = "book"
	MediaKindOther   MediaKind = "other"
)

// ProgressEntry tracks playback position and watched state for one library item.
// JSON field names match muxcore-ios UserDataModels.swift / media-ui-app userdata.ts.
type ProgressEntry struct {
	ID          string    `json:"id"`
	Kind        MediaKind `json:"kind"`
	Title       string    `json:"title"`
	PosterURL   string    `json:"poster_url,omitempty"`
	Href        string    `json:"href"`
	StreamURL   string    `json:"stream_url,omitempty"`
	PositionSec float64   `json:"positionSec"`
	DurationSec float64   `json:"durationSec"`
	UpdatedAt   string    `json:"updatedAt"`
	Watched     *bool     `json:"watched,omitempty"`
}

// FavoriteEntry is a starred library item.
type FavoriteEntry struct {
	ID        string    `json:"id"`
	Kind      MediaKind `json:"kind"`
	Title     string    `json:"title"`
	PosterURL string    `json:"poster_url,omitempty"`
	Href      string    `json:"href"`
	Year      *int      `json:"year,omitempty"`
}

// DisplayPrefs holds UI display preferences.
type DisplayPrefs struct {
	Theme                 string `json:"theme"`
	LibraryPageSize       int    `json:"libraryPageSize"`
	ShowWatchedIndicators bool   `json:"showWatchedIndicators"`
}

// HomePrefs controls home screen rows.
type HomePrefs struct {
	ShowContinueWatching bool `json:"showContinueWatching"`
	ShowFavorites        bool `json:"showFavorites"`
	ShowRecentRequests   bool `json:"showRecentRequests"`
	ShowNextUp           bool `json:"showNextUp"`
}

// PlaybackPrefs controls player behaviour.
type PlaybackPrefs struct {
	AutoplayNext     bool `json:"autoplayNext"`
	RememberPosition bool `json:"rememberPosition"`
	SkipIntroSec     int  `json:"skipIntroSec"`
}

// SubtitlePrefs holds subtitle defaults.
type SubtitlePrefs struct {
	Enabled  bool   `json:"enabled"`
	Language string `json:"language"`
	TextSize string `json:"textSize"`
}

// ControlPrefs holds input preferences.
type ControlPrefs struct {
	EnableKeyboardShortcuts bool `json:"enableKeyboardShortcuts"`
}

// UserPreferences mirrors the client-side prefs blob.
type UserPreferences struct {
	Display   DisplayPrefs  `json:"display"`
	Home      HomePrefs     `json:"home"`
	Playback  PlaybackPrefs `json:"playback"`
	Subtitles SubtitlePrefs `json:"subtitles"`
	Controls  ControlPrefs  `json:"controls"`
}

// DefaultPreferences returns client-compatible defaults.
func DefaultPreferences() UserPreferences {
	return UserPreferences{
		Display: DisplayPrefs{
			Theme:                 "dark",
			LibraryPageSize:       48,
			ShowWatchedIndicators: true,
		},
		Home: HomePrefs{
			ShowContinueWatching: true,
			ShowFavorites:        true,
			ShowRecentRequests:   true,
			ShowNextUp:           true,
		},
		Playback: PlaybackPrefs{
			AutoplayNext:     false,
			RememberPosition: true,
			SkipIntroSec:     0,
		},
		Subtitles: SubtitlePrefs{
			Enabled:  true,
			Language: "eng",
			TextSize: "md",
		},
		Controls: ControlPrefs{
			EnableKeyboardShortcuts: true,
		},
	}
}

// Playlist is a user-defined list of item IDs.
type Playlist struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	ItemIDs []string `json:"itemIds"`
}

// QueueItem is a playback queue entry.
type QueueItem struct {
	ID        string    `json:"id"`
	Kind      MediaKind `json:"kind"`
	Title     string    `json:"title"`
	Href      string    `json:"href"`
	StreamURL string    `json:"stream_url,omitempty"`
	PosterURL string    `json:"poster_url,omitempty"`
}

// Blob is the canonical per-user userdata document exchanged with clients.
type Blob struct {
	Progress  map[string]ProgressEntry `json:"progress,omitempty"`
	Favorites map[string]FavoriteEntry `json:"favorites,omitempty"`
	Prefs     *UserPreferences         `json:"prefs,omitempty"`
	Playlists []Playlist               `json:"playlists,omitempty"`
	Queue     []QueueItem              `json:"queue,omitempty"`
}

// EmptyBlob returns a blob with default preferences initialized.
func EmptyBlob() Blob {
	prefs := DefaultPreferences()
	return Blob{
		Progress:  map[string]ProgressEntry{},
		Favorites: map[string]FavoriteEntry{},
		Prefs:     &prefs,
		Playlists: []Playlist{},
		Queue:     []QueueItem{},
	}
}

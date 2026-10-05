package internal

import (
	"log/slog"
	"os"
	"path/filepath"
)

const dbFileName = "userdata.db"

// dataDir returns the module data directory: USERDATA_LOCAL_DATA_DIR, else
// <MUXCORE_DATA_DIR>/userdata, else ./data/userdata. Keeping the database
// under the data dir places household progress/favorites inside the paths
// covered by backups (ADR-0013).
func dataDir() string {
	if v := os.Getenv("USERDATA_LOCAL_DATA_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("MUXCORE_DATA_DIR"); v != "" {
		return filepath.Join(v, "userdata")
	}
	return filepath.Join("data", "userdata")
}

// defaultDBPath is the database path used when USERDATA_LOCAL_DB_PATH is unset.
func defaultDBPath() string {
	return filepath.Join(dataDir(), dbFileName)
}

// legacyDBPath is the pre-ADR-0013 default location ($HOME/.muxcore/userdata.db).
func legacyDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".muxcore", dbFileName)
}

// warnLegacyDB logs a warning when the new default database does not exist but
// a legacy one does. It never moves data; the operator must do so.
// It reports whether a warning was emitted.
func warnLegacyDB(newPath string) bool {
	legacy := legacyDBPath()
	if legacy == "" {
		return false
	}
	if _, err := os.Stat(newPath); err == nil {
		return false
	}
	if _, err := os.Stat(legacy); err != nil {
		return false
	}
	slog.Warn("userdata-local: legacy database found outside the data dir and will NOT be used; "+
		"household progress/favorites in it are not covered by backups. "+
		"Stop the module and move it (including any -wal/-shm files), then restart, or set USERDATA_LOCAL_DB_PATH to keep using it",
		"legacy_path", legacy,
		"new_path", newPath,
		"command", "mkdir -p '"+filepath.Dir(newPath)+"' && mv '"+legacy+"'* '"+filepath.Dir(newPath)+"/'")
	return true
}

package internal

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"USERDATA_LOCAL_DB_PATH", "USERDATA_LOCAL_DATA_DIR", "MUXCORE_DATA_DIR"} {
		t.Setenv(k, "")
	}
}

func TestDefaultDBPath(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", t.TempDir())
	m := NewModule(Config{})
	if want := filepath.Join("data", "userdata", "userdata.db"); m.dbPath != want {
		t.Fatalf("dbPath=%q want %q", m.dbPath, want)
	}
	t.Setenv("MUXCORE_DATA_DIR", "/srv/mx")
	if got, want := NewModule(Config{}).dbPath, "/srv/mx/userdata/userdata.db"; got != want {
		t.Fatalf("dbPath=%q want %q", got, want)
	}
	t.Setenv("USERDATA_LOCAL_DATA_DIR", "/srv/ud")
	if got, want := NewModule(Config{}).dbPath, "/srv/ud/userdata.db"; got != want {
		t.Fatalf("dbPath=%q want %q", got, want)
	}
}

func TestDBPathOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("USERDATA_LOCAL_DATA_DIR", "/srv/ud")
	t.Setenv("USERDATA_LOCAL_DB_PATH", "/custom/x.db")
	if got := NewModule(Config{}).dbPath; got != "/custom/x.db" {
		t.Fatalf("env override: %q", got)
	}
	if got := NewModule(Config{DBPath: "/cfg/y.db"}).dbPath; got != "/cfg/y.db" {
		t.Fatalf("config override: %q", got)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestLegacyDBWarning(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := t.TempDir()
	t.Setenv("USERDATA_LOCAL_DATA_DIR", dataDir)

	// No legacy file: silent.
	buf := captureLog(t)
	NewModule(Config{})
	if buf.Len() != 0 {
		t.Fatalf("unexpected log: %s", buf)
	}

	// Legacy file present, new absent: warn with exact command, data untouched.
	legacy := filepath.Join(home, ".muxcore", "userdata.db")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	NewModule(Config{})
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, legacy) ||
		!strings.Contains(out, "mv '"+legacy+"'* '"+dataDir+"/'") {
		t.Fatalf("missing warning/command: %s", out)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file must not be moved: %v", err)
	}

	// Explicit override: no warning.
	buf.Reset()
	NewModule(Config{DBPath: filepath.Join(dataDir, "o.db")})
	if buf.Len() != 0 {
		t.Fatalf("override should not warn: %s", buf)
	}

	// New db exists: no warning.
	if err := os.WriteFile(filepath.Join(dataDir, "userdata.db"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	NewModule(Config{})
	if buf.Len() != 0 {
		t.Fatalf("existing new db should not warn: %s", buf)
	}
}

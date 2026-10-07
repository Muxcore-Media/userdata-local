package internal_test

import (
	"context"
	"testing"

	"github.com/Muxcore-Media/core/pkg/contracts"
	"github.com/Muxcore-Media/userdata-local/internal"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
)

func TestModuleLifecycle(t *testing.T) {
	provider := auth.NewStaticProvider(map[string]contracts.Session{
		"test-token": {UserID: "alice", Roles: []string{"user"}},
	})
	m := internal.NewModule(internal.Config{
		GRPCAddr:     "127.0.0.1:0",
		HTTPAddr:     "127.0.0.1:0",
		DBPath:       t.TempDir() + "/userdata.db",
		AuthProvider: provider,
	})
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestModuleInfo(t *testing.T) {
	m := internal.NewModule(internal.Config{})
	info := m.Info()
	if info.ID != "userdata-local" {
		t.Errorf("ID=%q", info.ID)
	}
	foundUserdata, foundSettings, foundParental := false, false, false
	for _, c := range info.Capabilities {
		if c == "userdata.local" {
			foundUserdata = true
		}
		if c == "settings" {
			foundSettings = true
		}
		if c == "userdata.parental-policy.v1" {
			foundParental = true
		}
	}
	if !foundUserdata || !foundSettings || !foundParental {
		t.Fatalf("capabilities=%v", info.Capabilities)
	}
}

func TestSettingsDBPath(t *testing.T) {
	m := internal.NewModule(internal.Config{DBPath: "/tmp/userdata-test.db"})
	if err := m.UpdateSetting("db_path", "/var/lib/muxcore/userdata.db"); err != nil {
		t.Fatalf("UpdateSetting: %v", err)
	}
	defs := m.Settings()
	if defs[0].Value != "/var/lib/muxcore/userdata.db" {
		t.Fatalf("value=%q", defs[0].Value)
	}
}

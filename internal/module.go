package internal

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"

	"github.com/Muxcore-Media/core/pkg/contracts"
	modulesdk "github.com/Muxcore-Media/core/sdk/go/module"
	"github.com/Muxcore-Media/userdata-local/internal/server"
	"github.com/Muxcore-Media/userdata-local/internal/store"
)

const (
	moduleID      = "userdata-local"
	moduleVersion = "0.1.0"
)

// Module is the MuxCore userdata-local sidecar.
type Module struct {
	store    *store.Store
	srv      *server.Server
	grpcSrv  *grpc.Server
	httpSrv  *http.Server
	grpcLis  net.Listener
	httpLis  net.Listener
	cfgMu    sync.RWMutex
	id       string
	grpcAddr string
	httpAddr string
	dbPath   string
}

// Config holds module settings. Non-empty fields override environment.
type Config struct {
	ID       string
	GRPCAddr string
	HTTPAddr string
	DBPath   string
}

// NewModule constructs the module with env fallbacks.
func NewModule(cfg Config) *Module {
	if cfg.ID == "" {
		cfg.ID = moduleID
	}
	if cfg.GRPCAddr == "" {
		cfg.GRPCAddr = os.Getenv("USERDATA_LOCAL_GRPC_ADDR")
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = os.Getenv("USERDATA_LOCAL_HTTP_ADDR")
	}
	if cfg.DBPath == "" {
		cfg.DBPath = os.Getenv("USERDATA_LOCAL_DB_PATH")
	}
	if cfg.GRPCAddr == "" {
		cfg.GRPCAddr = ":9703"
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":9701"
	}
	if cfg.DBPath == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			cfg.DBPath = "userdata.db"
		} else {
			cfg.DBPath = filepath.Join(home, ".muxcore", "userdata.db")
		}
	}
	return &Module{
		id:       cfg.ID,
		grpcAddr: cfg.GRPCAddr,
		httpAddr: cfg.HTTPAddr,
		dbPath:   cfg.DBPath,
	}
}

func (m *Module) Info() contracts.ModuleInfo {
	return contracts.ModuleInfo{
		ID:           m.id,
		Name:         "Userdata Local",
		Version:      moduleVersion,
		Roles:        []string{"household", "media"},
		Description:  "Durable per-user household library state (progress, favorites, continue-watching)",
		Author:       "MuxCore",
		Capabilities: []string{"userdata.local", "settings"},
		HTTPAddr:     m.httpAddr,
	}
}

func (m *Module) Init(ctx context.Context) error {
	if dir := filepath.Dir(m.dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create db dir: %w", err)
		}
	}
	var err error
	m.store, err = store.New(m.dbPath)
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}
	m.srv = server.New(m.store)

	m.grpcLis, err = net.Listen("tcp", m.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen gRPC %s: %w", m.grpcAddr, err)
	}
	m.httpLis, err = net.Listen("tcp", m.httpAddr)
	if err != nil {
		return fmt.Errorf("listen HTTP %s: %w", m.httpAddr, err)
	}
	slog.Info("userdata-local initialized", "grpc", m.grpcAddr, "http", m.httpAddr, "db", m.dbPath)
	return nil
}

func (m *Module) Start(ctx context.Context) error {
	m.grpcSrv = grpc.NewServer()
	m.srv.RegisterWithGRPC(m.grpcSrv)
	modulesdk.RegisterSettings(m.grpcSrv, m.id, m)
	go func() {
		slog.Info("userdata-local gRPC started", "addr", m.grpcAddr)
		if err := m.grpcSrv.Serve(m.grpcLis); err != nil {
			slog.Error("userdata-local gRPC error", "error", err)
		}
	}()

	mux := http.NewServeMux()
	m.srv.RegisterRoutes(mux)
	m.httpSrv = &http.Server{Handler: mux}
	go func() {
		slog.Info("userdata-local HTTP started", "addr", m.httpAddr)
		if err := m.httpSrv.Serve(m.httpLis); err != nil && err != http.ErrServerClosed {
			slog.Error("userdata-local HTTP error", "error", err)
		}
	}()
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	if m.grpcSrv != nil {
		m.grpcSrv.GracefulStop()
	}
	if m.httpSrv != nil {
		_ = m.httpSrv.Shutdown(ctx)
	}
	if m.store != nil {
		_ = m.store.Close()
	}
	slog.Info("userdata-local stopped")
	return nil
}

func (m *Module) Health(ctx context.Context) error {
	if m.store == nil {
		return fmt.Errorf("not initialized")
	}
	return nil
}

// Store exposes the backing store for tests.
func (m *Module) Store() *store.Store {
	return m.store
}

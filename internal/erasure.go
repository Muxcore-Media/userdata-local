package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"google.golang.org/grpc"

	discoveryv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/discovery/v1"
	modulesdk "github.com/Muxcore-Media/core/sdk/go/module"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure"
	"github.com/Muxcore-Media/core/sdk/go/module/meshtls"
	"github.com/Muxcore-Media/userdata-local/internal/store"
)

// erasureOwner is userdata-local's ADR-0035 personal-data owner: it applies
// ledger tombstones to the SQLite store and nothing else. It has no input
// other than the Reconciler, which reads only the verified identity
// provider's ledger.
type erasureOwner struct {
	store *store.Store
	id    string
}

var (
	_ erasure.Owner    = (*erasureOwner)(nil)
	_ erasure.Verifier = (*erasureOwner)(nil)
)

// ModuleID implements erasure.Owner.
func (o *erasureOwner) ModuleID() string { return o.id }

// Applied implements erasure.Owner.
func (o *erasureOwner) Applied(ctx context.Context, erasureID string) (bool, error) {
	return o.store.ErasureApplied(ctx, erasureID)
}

// Apply implements erasure.Owner: one store transaction deletes/anonymises and
// records the erasure.
func (o *erasureOwner) Apply(ctx context.Context, t erasure.Tombstone) (erasure.Counts, error) {
	counts, err := o.store.EraseUser(ctx, t.ErasureID, t.UserID, t.TenantID)
	if err != nil {
		return nil, err
	}
	return erasure.Counts(counts), nil
}

// Verify implements erasure.Verifier: rows still carrying the user id.
func (o *erasureOwner) Verify(ctx context.Context, t erasure.Tombstone) (int, error) {
	return o.store.CountUserRows(ctx, t.UserID)
}

// ErasureSweepInterval is the environment variable read for the sweep period.
const ErasureSweepInterval = erasure.EnvSweepInterval

// erasureRequired reports whether the profile makes the reconciler mandatory:
// household (and its alias staging) must not run without it.
func erasureRequired() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MUXCORE_PROFILE"))) {
	case "household", "staging":
		return true
	}
	return false
}

// setupErasure builds the reconciler for the module. It runs from Init, which
// modulesdk.Run calls after mesh enrollment, so MUXCORE_TLS_CERT/KEY/CA point
// at the module's own identity. With a core connection it discovers the
// exclusive identity provider through core; without one the reconciler is not
// started (dev/tests), which household refuses.
func (m *Module) setupErasure() error {
	dialer := m.erasureDialer
	if dialer == nil {
		if strings.TrimSpace(os.Getenv("MUXCORE_GRPC_ADDR")) == "" {
			if erasureRequired() {
				return errors.New("erasure reconciler: household profile requires a core connection (MUXCORE_GRPC_ADDR)")
			}
			slog.Warn("userdata-local erasure reconciler disabled: no core connection (MUXCORE_GRPC_ADDR unset); user erasures from the identity ledger are not applied")
			return nil
		}
		conn, err := modulesdk.Connect(modulesdk.ConnectConfig{Insecure: meshtls.Insecure()})
		if err != nil {
			return fmt.Errorf("erasure reconciler: connect to core: %w", err)
		}
		m.coreConn = conn
		dialer = &erasure.ProviderDialer{Discovery: discoveryClient{conn: conn}}
	}
	cfg := erasure.Config{
		Owner:    &erasureOwner{store: m.store, id: m.id},
		Dialer:   dialer,
		Logger:   slog.Default(),
		Interval: m.erasureInterval,
	}
	if m.erasureTune != nil {
		m.erasureTune(&cfg)
	}
	rec, err := erasure.New(cfg)
	if err != nil {
		if m.coreConn != nil {
			_ = m.coreConn.Close()
			m.coreConn = nil
		}
		return err
	}
	m.reconciler = rec
	return nil
}

// discoveryClient adapts a core connection to erasure.CapabilityFinder.
type discoveryClient struct{ conn *grpc.ClientConn }

func (d discoveryClient) FindByCapability(ctx context.Context, in *discoveryv1.FindByCapabilityRequest, opts ...grpc.CallOption) (*discoveryv1.FindByCapabilityResponse, error) {
	return discoveryv1.NewDiscoveryServiceClient(d.conn).FindByCapability(ctx, in, opts...)
}

// startErasure runs the reconciler until stopErasure.
func (m *Module) startErasure() {
	if m.reconciler == nil || m.erasureCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.erasureCancel, m.erasureDone = cancel, done
	rec := m.reconciler
	go func() {
		defer close(done)
		if err := rec.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("userdata-local erasure reconciler stopped", "error", err)
		}
	}()
}

// stopErasure cancels the reconciler and waits for it, so no sweep touches the
// store after Stop closes it.
func (m *Module) stopErasure(ctx context.Context) {
	if m.erasureCancel != nil {
		m.erasureCancel()
		select {
		case <-m.erasureDone:
		case <-ctx.Done():
			// Bounded by the caller's shutdown deadline; sweeps honour ctx
			// cancellation, so this only trips on a hung local database.
			slog.Warn("userdata-local erasure reconciler did not stop before the shutdown deadline")
		}
		m.erasureCancel, m.erasureDone = nil, nil
	}
	m.reconciler = nil
	if m.coreConn != nil {
		_ = m.coreConn.Close()
		m.coreConn = nil
	}
}

// Reconciler exposes the erasure reconciler (nil when disabled) for tests and
// operator triggers.
func (m *Module) Reconciler() *erasure.Reconciler { return m.reconciler }

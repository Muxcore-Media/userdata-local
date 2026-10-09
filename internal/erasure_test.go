package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/Muxcore-Media/core/pkg/contracts"
	authv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/auth/v1"
	discoveryv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/discovery/v1"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure/erasuretest"
	"github.com/Muxcore-Media/core/sdk/go/module/meshtls"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	"github.com/Muxcore-Media/userdata-local/parental"
)

const (
	testProviderID = "auth-local"
	testOwnerID    = "userdata-local"
)

func clearMeshEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MUXCORE_INSECURE_DISABLE_TLS", "MUXCORE_DEV_TLS_SKIP", "MUXCORE_GRPC_INSECURE",
		meshtls.EnvTLSCert, meshtls.EnvTLSKey, meshtls.EnvTLSCA, meshtls.EnvTLSServerName,
		"MUXCORE_PROFILE", "MUXCORE_MESH_DIAL_LOCAL", "MUXCORE_GRPC_ADDR", erasure.EnvSweepInterval} {
		t.Setenv(k, "")
	}
}

func devEnv(t *testing.T) {
	t.Helper()
	clearMeshEnv(t)
	t.Setenv("MUXCORE_PROFILE", "dev")
	t.Setenv("MUXCORE_INSECURE_DISABLE_TLS", "true")
}

// seedErasureDB writes victim and bystander data: blobs for victim, bystander
// and other-tenant-user; policies in tenant home (victim, bystander, and one
// last written by the victim) and in tenant elsewhere.
func seedErasureDB(t *testing.T, path string) {
	t.Helper()
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, u := range []string{"victim", "bystander", "other-tenant-user"} {
		if _, _, err := st.Put(u, models.Blob{Favorites: map[string]models.FavoriteEntry{"f": {ID: "f", Kind: models.MediaKindMovie, Title: "F", Href: "/f"}}}); err != nil {
			t.Fatal(err)
		}
	}
	up := parental.Update{Policy: parental.Policy{Version: 1, Mode: "unrestricted"}}
	for _, p := range []struct{ tenant, user, actor string }{
		{"home", "victim", "parent"},
		{"home", "bystander", "victim"},
		{"home", "bystander2", "parent"},
		{"elsewhere", "other-tenant-user", "other-admin"},
	} {
		if _, err := st.PutParentalPolicy(context.Background(), parental.Scope{TenantID: p.tenant, UserID: p.user}, p.actor, up); err != nil {
			t.Fatal(err)
		}
	}
}

// userRows counts rows by user id in user_blobs and parental_policies via an
// independent connection.
func userRows(t *testing.T, path string) (blobs map[string]int, policies map[string]int, applied int) {
	t.Helper()
	db := openRaw(t, path)
	blobs, policies = map[string]int{}, map[string]int{}
	collect := func(query string, into map[string]int) {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			into[k]++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	collect(`SELECT user_id FROM user_blobs`, blobs)
	collect(`SELECT user_id FROM parental_policies`, policies)
	if err := db.QueryRow(`SELECT COUNT(*) FROM erasure_applied`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	return blobs, policies, applied
}

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fakeCore serves a DiscoveryService answering the identity capability.
type fakeCore struct {
	discoveryv1.UnimplementedDiscoveryServiceServer
	disc *erasuretest.Discovery
}

func (f fakeCore) FindByCapability(ctx context.Context, in *discoveryv1.FindByCapabilityRequest) (*discoveryv1.FindByCapabilityResponse, error) {
	return f.disc.FindByCapability(ctx, in)
}

func serveFakeCore(t *testing.T, disc *erasuretest.Discovery) string {
	t.Helper()
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	discoveryv1.RegisterDiscoveryServiceServer(gs, fakeCore{disc: disc})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

func fastTune(c *erasure.Config) {
	c.AckBackoff = time.Millisecond
	c.RetryBackoff = 50 * time.Millisecond
	c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	c.CallTimeout = 5 * time.Second
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func userdataRequest(t *testing.T, m *Module, method, path, token, user, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, "http://"+m.httpLis.Addr().String()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(auth.UserIDHeader, user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func staticTokens() contracts.AuthProvider {
	return auth.NewStaticProvider(map[string]contracts.Session{
		"victim-token":    {UserID: "victim", Roles: []string{"user"}},
		"bystander-token": {UserID: "bystander", Roles: []string{"user"}},
	})
}

// TestErasureLifecycleThroughCoreDiscovery runs the real wiring: the module
// connects to a (fake) core, discovers the identity provider through it,
// sweeps at startup, refuses late writes, survives a restart and stops its
// reconciler goroutine on Stop.
func TestErasureLifecycleThroughCoreDiscovery(t *testing.T) {
	devEnv(t)
	dbPath := filepath.Join(t.TempDir(), "userdata.db")
	seedErasureDB(t, dbPath)

	provider := erasuretest.NewProvider()
	provider.Allowed = map[string]bool{testOwnerID: true}
	providerAddr := erasuretest.ServePlain(t, provider)
	coreAddr := serveFakeCore(t, erasuretest.NewDiscovery(erasuretest.Module(testProviderID, providerAddr)))
	t.Setenv("MUXCORE_GRPC_ADDR", coreAddr)
	t.Setenv(erasure.EnvSweepInterval, "1m")
	eid := provider.AddErasure("victim", "home")

	ctx := context.Background()
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: dbPath, AuthProvider: staticTokens(), ErasureTune: fastTune})
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Reconciler() == nil {
		t.Fatal("reconciler not configured despite a core connection")
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := m.erasureDone
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = m.Stop(ctx)
		}
	})

	// The startup sweep applies and acknowledges.
	eventually(t, "startup sweep acknowledgement", func() bool {
		a, ok := provider.Latest(eid, testOwnerID)
		return ok && a.Outcome == authv1.ErasureOutcome_ERASURE_OUTCOME_OK
	})
	ack, _ := provider.Latest(eid, testOwnerID)
	if ack.Counts[store.CountUserBlobs] != 1 || ack.Counts[store.CountParentalPolicies] != 1 || ack.Counts[store.CountParentalPoliciesAnonymise] != 1 {
		t.Errorf("ack counts = %v", ack.Counts)
	}
	blobs, policies, applied := userRows(t, dbPath)
	if blobs["victim"] != 0 || policies["victim"] != 0 || applied != 1 {
		t.Errorf("victim rows left: blobs=%v policies=%v applied=%d", blobs, policies, applied)
	}
	for _, u := range []string{"bystander", "other-tenant-user"} {
		if blobs[u] != 1 {
			t.Errorf("bystander blob %s lost: %v", u, blobs)
		}
	}
	for _, u := range []string{"bystander", "bystander2", "other-tenant-user"} {
		if policies[u] != 1 {
			t.Errorf("bystander policy %s lost: %v", u, policies)
		}
	}
	var by string
	if err := openRaw(t, dbPath).QueryRow(`SELECT updated_by FROM parental_policies WHERE user_id='bystander'`).Scan(&by); err != nil || by != store.AnonymisedActor {
		t.Errorf("bystander policy updated_by=%q err=%v", by, err)
	}

	// Second sweep: no-op (applied and already acknowledged OK).
	res, err := m.Reconciler().SweepOnce(ctx)
	if err != nil || res.Applied != 0 || res.Skipped != 1 {
		t.Fatalf("second sweep: %+v %v", res, err)
	}

	// Late writes are refused with the stable code and create nothing.
	status, body := userdataRequest(t, m, http.MethodPut, "/api/userdata", "victim-token", "victim", `{"favorites":{"z":{"id":"z"}}}`)
	var code map[string]string
	if status != http.StatusGone || json.Unmarshal([]byte(body), &code) != nil || code["code"] != "userdata.account_erased" {
		t.Fatalf("late PUT: %d %s", status, body)
	}
	if status, body := userdataRequest(t, m, http.MethodPut, "/api/userdata", "bystander-token", "bystander", `{}`); status != http.StatusOK {
		t.Fatalf("bystander PUT: %d %s", status, body)
	}
	if blobs, _, _ := userRows(t, dbPath); blobs["victim"] != 0 {
		t.Fatalf("late PUT resurrected data: %v", blobs)
	}

	// Stop: the reconciler goroutine ends before the store closes.
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stopped = true
	select {
	case <-done:
	default:
		t.Fatal("reconciler goroutine still running after Stop")
	}
	if m.Reconciler() != nil || m.coreConn != nil {
		t.Fatal("reconciler or core connection retained after Stop")
	}

	// Restart without any provider: refusal comes from the table, not memory.
	t.Setenv("MUXCORE_GRPC_ADDR", "")
	m2 := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: dbPath, AuthProvider: staticTokens()})
	if err := m2.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Stop(ctx) })
	if m2.Reconciler() != nil {
		t.Fatal("reconciler started without a core connection")
	}
	status, body = userdataRequest(t, m2, http.MethodPut, "/api/userdata", "victim-token", "victim", `{"favorites":{"z":{"id":"z"}}}`)
	if status != http.StatusGone {
		t.Fatalf("late PUT after restart: %d %s", status, body)
	}
}

// TestErasureMidTransactionFailureRollsBackAndNextSweepCompletes injects a
// failure after the deletes (an aborting trigger on the applied-record
// insert): nothing is partially erased and no applied row exists. The next
// sweep completes.
func TestErasureMidTransactionFailureRollsBackAndNextSweepCompletes(t *testing.T) {
	devEnv(t)
	dbPath := filepath.Join(t.TempDir(), "userdata.db")
	seedErasureDB(t, dbPath)
	provider := erasuretest.NewProvider()
	providerAddr := erasuretest.ServePlain(t, provider)
	dialer := &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery(erasuretest.Module(testProviderID, providerAddr))}
	eid := provider.AddErasure("victim", "home")

	ctx := context.Background()
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: dbPath, AuthProvider: staticTokens(), ErasureDialer: dialer, ErasureTune: fastTune})
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(ctx) })

	raw := openRaw(t, dbPath)
	if _, err := raw.Exec(`CREATE TRIGGER inject_failure BEFORE INSERT ON erasure_applied BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	res, err := m.Reconciler().SweepOnce(ctx)
	if err == nil || res.Failed != 1 || res.Applied != 0 {
		t.Fatalf("sweep with injected failure: %+v %v", res, err)
	}
	a, ok := provider.Latest(eid, testOwnerID)
	if !ok || a.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_FAILED || a.Detail != erasure.DetailApplyFailed {
		t.Fatalf("ack after failure: %+v %v", a, ok)
	}
	blobs, policies, applied := userRows(t, dbPath)
	if blobs["victim"] != 1 || policies["victim"] != 1 || applied != 0 {
		t.Fatalf("partial erasure: blobs=%v policies=%v applied=%d", blobs, policies, applied)
	}
	var by string
	if err := raw.QueryRow(`SELECT updated_by FROM parental_policies WHERE user_id='bystander'`).Scan(&by); err != nil || by != "victim" {
		t.Fatalf("anonymisation not rolled back: %q %v", by, err)
	}

	if _, err := raw.Exec(`DROP TRIGGER inject_failure`); err != nil {
		t.Fatal(err)
	}
	res, err = m.Reconciler().SweepOnce(ctx)
	if err != nil || res.Applied != 1 || res.Acked != 1 {
		t.Fatalf("next sweep: %+v %v", res, err)
	}
	if a, _ := provider.Latest(eid, testOwnerID); a.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_OK {
		t.Fatalf("ack after retry: %+v", a)
	}
	blobs, policies, applied = userRows(t, dbPath)
	if blobs["victim"] != 0 || policies["victim"] != 0 || applied != 1 || blobs["bystander"] != 1 {
		t.Fatalf("after retry: blobs=%v policies=%v applied=%d", blobs, policies, applied)
	}
	n, err := m.store.CountUserRows(ctx, "victim")
	if err != nil || n != 0 {
		t.Fatalf("post-condition = %d, %v", n, err)
	}
}

// TestErasureLedgerFromWrongCertificateNeverActedOn: the provider's TLS
// certificate chains to the mesh CA and even carries the provider id as a SAN,
// but its CN is another module: the ledger must not be read, so nothing is
// erased.
func TestErasureLedgerFromWrongCertificateNeverActedOn(t *testing.T) {
	clearMeshEnv(t)
	dbPath := filepath.Join(t.TempDir(), "userdata.db")
	seedErasureDB(t, dbPath)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	pki := erasuretest.NewPKI(t)
	provider := erasuretest.NewProvider()
	provider.AddErasure("victim", "home")
	provider.AddErasure("bystander", "home")
	impCert, impKey := pki.Issue(t, "mallory", testProviderID, "localhost")
	addr := erasuretest.ServeTLS(t, provider, impCert, impKey, pki.CAFile)
	cc, ck := pki.Issue(t, testOwnerID)
	dialer := &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery(erasuretest.Module(testProviderID, addr)), CertFile: cc, KeyFile: ck, CAFile: pki.CAFile}
	rec, err := erasure.New(erasure.Config{Owner: &erasureOwner{store: st, id: testOwnerID}, Dialer: dialer, Interval: time.Minute,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), CallTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rec.SweepOnce(context.Background())
	if err == nil || res.Seen != 0 || res.Applied != 0 {
		t.Fatalf("sweep against impersonating provider: %+v %v", res, err)
	}
	blobs, policies, applied := userRows(t, dbPath)
	if blobs["victim"] != 1 || blobs["bystander"] != 1 || policies["victim"] != 1 || applied != 0 {
		t.Fatalf("impersonated ledger changed data: blobs=%v policies=%v applied=%d", blobs, policies, applied)
	}
	if list, _ := provider.Calls(); list != 0 {
		t.Fatalf("ledger RPC reached the impersonator %d times", list)
	}

	// With the genuine provider certificate the same ledger is applied over
	// real mTLS and acknowledged under the verified CN.
	genuineCert, genuineKey := pki.Issue(t, testProviderID)
	provider.Allowed = map[string]bool{testOwnerID: true}
	good := erasuretest.ServeTLS(t, provider, genuineCert, genuineKey, pki.CAFile)
	dialer.Discovery = erasuretest.NewDiscovery(erasuretest.Module(testProviderID, good))
	res, err = rec.SweepOnce(context.Background())
	if err != nil || res.Applied != 2 || res.Acked != 2 {
		t.Fatalf("genuine sweep: %+v %v", res, err)
	}
	blobs, policies, applied = userRows(t, dbPath)
	if len(blobs) != 1 || blobs["other-tenant-user"] != 1 || applied != 2 || policies["victim"] != 0 {
		t.Fatalf("after genuine sweep: blobs=%v policies=%v applied=%d", blobs, policies, applied)
	}
}

// TestErasureProviderUnreachableErasesNothing: discovery or the provider
// failing is never an erasure.
func TestErasureProviderUnreachableErasesNothing(t *testing.T) {
	devEnv(t)
	dbPath := filepath.Join(t.TempDir(), "userdata.db")
	seedErasureDB(t, dbPath)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	disc := erasuretest.NewDiscovery()
	rec, err := erasure.New(erasure.Config{Owner: &erasureOwner{store: st, id: testOwnerID}, Dialer: &erasure.ProviderDialer{Discovery: disc},
		Interval: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), CallTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.SweepOnce(context.Background()); err == nil {
		t.Fatal("sweep without a provider succeeded")
	}
	// A registered provider whose address refuses connections.
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := lis.Addr().String()
	_ = lis.Close()
	disc.Set("identity", erasuretest.Module(testProviderID, dead))
	if _, err := rec.SweepOnce(context.Background()); err == nil {
		t.Fatal("sweep against a dead provider succeeded")
	}
	blobs, policies, applied := userRows(t, dbPath)
	if blobs["victim"] != 1 || policies["victim"] != 1 || applied != 0 {
		t.Fatalf("data changed without a ledger: blobs=%v policies=%v applied=%d", blobs, policies, applied)
	}
}

// TestForgedErasureInputsCannotErase: the ledger of the verified identity
// provider is the only authority. A published identity.user.deleted event,
// any HTTP call and any gRPC call on the module leave the data untouched.
func TestForgedErasureInputsCannotErase(t *testing.T) {
	devEnv(t)
	dbPath := filepath.Join(t.TempDir(), "userdata.db")
	seedErasureDB(t, dbPath)
	ctx := context.Background()
	// Empty ledger.
	provider := erasuretest.NewProvider()
	providerAddr := erasuretest.ServePlain(t, provider)
	dialer := &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery(erasuretest.Module(testProviderID, providerAddr))}
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: dbPath, AuthProvider: staticTokens(), ErasureDialer: dialer, ErasureTune: fastTune})
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(ctx) })

	// A fake identity.user.deleted event on a bus the module is attached to
	// (it is attached to none: the module has no subscription code).
	bus := &captureBus{}
	payload, _ := json.Marshal(map[string]string{"user_id": "victim"})
	bus.Publish(contracts.Event{ID: "e1", Type: "identity.user.deleted", Source: testProviderID, Payload: payload})
	bus.Publish(contracts.Event{ID: "e2", Type: "identity.erasure.recorded", Source: testProviderID})

	for _, c := range []struct{ method, path, body string }{
		{http.MethodDelete, "/api/userdata", ""},
		{http.MethodPost, "/api/userdata", `{"user_id":"victim"}`},
		{http.MethodPost, "/api/erase", `{"user_id":"victim"}`},
		{http.MethodPost, "/api/erasure", `{"erasure_id":"x","user_id":"victim"}`},
		{http.MethodDelete, "/api/parental-policy", ""},
		{http.MethodPost, "/events", `{"type":"identity.user.deleted","user_id":"victim"}`},
	} {
		status, _ := userdataRequest(t, m, c.method, c.path, "victim-token", "victim", c.body)
		if status < 400 {
			t.Errorf("%s %s answered %d", c.method, c.path, status)
		}
	}
	for svc := range m.grpcSrv.GetServiceInfo() {
		if strings.Contains(strings.ToLower(svc), "erase") || strings.Contains(strings.ToLower(svc), "event") {
			t.Errorf("unexpected gRPC service %s", svc)
		}
	}
	if _, err := m.Reconciler().SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	blobs, policies, applied := userRows(t, dbPath)
	if blobs["victim"] != 1 || policies["victim"] != 1 || applied != 0 {
		t.Fatalf("forged input changed data: blobs=%v policies=%v applied=%d", blobs, policies, applied)
	}
}

type captureBus struct{ published atomic.Int32 }

func (b *captureBus) Publish(contracts.Event) { b.published.Add(1) }

// TestNoEventSubscriptionPath is a structural guard for the same rule: no
// production source of the module imports an event client or names the draft
// identity.user.deleted event, so the reconciler is the only erasure input.
func TestNoEventSubscriptionPath(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "proto" || d.Name() == "testdata" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, "core/sdk/go/client") || strings.Contains(p, "/events") {
				t.Errorf("%s imports %s", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetupErasureConfiguration(t *testing.T) {
	ctx := context.Background()
	t.Run("household requires a core connection", func(t *testing.T) {
		clearMeshEnv(t)
		t.Setenv("MUXCORE_PROFILE", "household")
		m := NewModule(Config{DBPath: filepath.Join(t.TempDir(), "u.db")})
		if err := m.setupErasure(); err == nil {
			t.Fatal("household without a core connection started")
		}
	})
	t.Run("dev without core disables with a warning", func(t *testing.T) {
		devEnv(t)
		m := NewModule(Config{DBPath: filepath.Join(t.TempDir(), "u.db")})
		if err := m.setupErasure(); err != nil || m.Reconciler() != nil {
			t.Fatalf("dev: %v %v", err, m.Reconciler())
		}
	})
	t.Run("invalid sweep interval is a startup error", func(t *testing.T) {
		devEnv(t)
		t.Setenv(erasure.EnvSweepInterval, "soon")
		dialer := &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery()}
		m := NewModule(Config{DBPath: filepath.Join(t.TempDir(), "u.db"), ErasureDialer: dialer})
		if err := m.setupErasure(); err == nil {
			t.Fatal("invalid ERASURE_SWEEP_INTERVAL accepted")
		}
	})
	t.Run("interval from environment", func(t *testing.T) {
		devEnv(t)
		t.Setenv(erasure.EnvSweepInterval, "2m")
		dialer := &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery()}
		m := NewModule(Config{DBPath: filepath.Join(t.TempDir(), "u.db"), ErasureDialer: dialer})
		if err := m.setupErasure(); err != nil || m.Reconciler() == nil {
			t.Fatalf("setup: %v", err)
		}
		m.stopErasure(ctx)
	})
}

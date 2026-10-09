package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muxcore-Media/core/pkg/contracts"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure/erasuretest"
	"github.com/Muxcore-Media/userdata-local/httpclient"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/testtls"
	"github.com/Muxcore-Media/userdata-local/parental"
)

type transportAuth struct {
	*auth.StaticProvider
	mu         sync.Mutex
	identities map[string]auth.PolicyIdentity
	accounts   map[string]parental.Scope
	calls      atomic.Int64
}

func (p *transportAuth) ValidatePolicyIdentity(_ context.Context, token string) (auth.PolicyIdentity, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	identity, ok := p.identities[token]
	if !ok {
		return auth.PolicyIdentity{}, auth.ErrPolicyUnauthenticated
	}
	return identity, nil
}
func (p *transportAuth) FindPolicyAccount(_ context.Context, _, id string) (parental.Scope, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	account, ok := p.accounts[id]
	if !ok {
		return parental.Scope{}, auth.ErrPolicyTargetNotFound
	}
	return account, nil
}
func (p *transportAuth) Validate(ctx context.Context, token string) (contracts.Session, error) {
	id, err := p.ValidatePolicyIdentity(ctx, token)
	return contracts.Session{UserID: id.UserID, Roles: id.Roles}, err
}

func freshTransportAuth() *transportAuth {
	return &transportAuth{StaticProvider: auth.NewStaticProvider(nil),
		identities: map[string]auth.PolicyIdentity{
			"admin": {Scope: parental.Scope{UserID: "parent", TenantID: "home"}, Roles: []string{"admin"}},
			"kid":   {Scope: parental.Scope{UserID: "kid", TenantID: "home"}, Roles: []string{"user"}},
		}, accounts: map[string]parental.Scope{"kid": {UserID: "kid", TenantID: "home"}, "other": {UserID: "other", TenantID: "elsewhere"}}}
}

func transportEnv(t *testing.T, ca *testtls.CA, provider testtls.Identity) {
	t.Helper()
	for _, name := range []string{"MUXCORE_INSECURE_DISABLE_TLS", "MUXCORE_GRPC_INSECURE", "MUXCORE_DEV_TLS_SKIP", "MUXCORE_TLS_DIR", "MUXCORE_CA_EXPORT_DIR", "USERDATA_TLS_CERT", "USERDATA_TLS_KEY", "USERDATA_TLS_CA"} {
		t.Setenv(name, "")
	}
	t.Setenv("MUXCORE_PROFILE", "household")
	t.Setenv("MUXCORE_TLS_CERT", provider.CertFile)
	t.Setenv("MUXCORE_TLS_KEY", provider.KeyFile)
	t.Setenv("MUXCORE_TLS_CA", ca.File)
}

func TestHouseholdHTTPModuleBearerRevisionAndPersistence(t *testing.T) {
	ca := testtls.NewCA(t)
	provider := ca.Issue(t, "userdata-local")
	transportEnv(t, ca, provider)
	identity := freshTransportAuth()
	db := filepath.Join(t.TempDir(), "userdata.db")
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: db, AuthProvider: identity, ErasureDialer: noProviderDialer()})
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(ctx) })
	clientFor := func(cn string) *httpclient.Client {
		id := ca.Issue(t, cn)
		c, err := httpclient.New(httpclient.Config{Origin: "https://" + m.httpLis.Addr().String(), ModuleID: cn, Profile: "household", CertFile: id.CertFile, KeyFile: id.KeyFile, CAFile: ca.File})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.CloseIdleConnections)
		return c
	}
	media, admin, self, unknown := clientFor("media-ui"), clientFor("admin-ui"), clientFor("userdata-local"), clientFor("jellyfin")
	request := func(c *httpclient.Client, op httpclient.Operation, token, target, body string) (int, []byte, error) {
		headers := http.Header{"X-Muxcore-User-Id": {target}, "Content-Type": {"application/json"}, "X-Caller-Id": {"admin-ui"}, "X-Tenant-Id": {"attacker"}}
		if token != "" {
			headers.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.Do(ctx, op, headers, strings.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return resp.StatusCode, data, err
	}
	check := func(c *httpclient.Client, op httpclient.Operation, token, target, body string, want int) []byte {
		t.Helper()
		status, data, err := request(c, op, token, target, body)
		if err != nil || status != want {
			t.Fatalf("operation %v status=%d want=%d body=%s err=%v", op, status, want, data, err)
		}
		return data
	}
	unconfigured := check(media, httpclient.GetPolicy, "kid", "kid", "", 200)
	if !strings.Contains(string(unconfigured), `"state":"unconfigured"`) {
		t.Fatalf("absence: %s", unconfigured)
	}
	const update = `{"expected_revision":0,"policy":{"version":1,"mode":"restricted","rules":{"kids_mode":true,"max_rating":"PG","blocked_tags":["horror"],"allowed_tags":[],"allow_unrated":false}}}`
	before := identity.calls.Load()
	for _, c := range []*httpclient.Client{media, self, unknown} {
		_, _, err := request(c, httpclient.PutPolicy, "admin", "kid", update)
		var unavailable *httpclient.UnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != httpclient.ReasonModuleForbidden {
			t.Fatalf("module denial: %v", err)
		}
	}
	if identity.calls.Load() != before {
		t.Fatal("admission failure invoked auth")
	}
	doc, err := m.store.GetParentalPolicy(ctx, parental.Scope{UserID: "kid", TenantID: "home"})
	if err != nil || doc.Revision != 0 {
		t.Fatal("admission failure wrote storage")
	}
	check(admin, httpclient.PutPolicy, "kid", "kid", update, 403)
	check(admin, httpclient.PutPolicy, "", "kid", update, 401)
	check(media, httpclient.GetPolicy, "", "kid", "", 401)
	check(media, httpclient.GetUserdata, "", "kid", "", 401)
	check(admin, httpclient.PutPolicy, "admin", "other", update, 403)
	check(admin, httpclient.GetPolicy, "admin", "absent", "", 404)
	created := check(admin, httpclient.PutPolicy, "admin", "kid", update, 200)
	if err := json.Unmarshal(created, &doc); err != nil || doc.Revision != 1 || doc.Policy == nil || doc.Policy.Rules.MaxRating != "PG" {
		t.Fatalf("created policy: %s %v", created, err)
	}
	check(admin, httpclient.PutPolicy, "admin", "kid", update, 409)
	read := check(media, httpclient.GetPolicy, "kid", "kid", "", 200)
	if err := json.Unmarshal(read, &doc); err != nil || doc.Revision != 1 || doc.TenantID != "home" {
		t.Fatalf("policy read: %s %v", read, err)
	}
	check(media, httpclient.GetPolicy, "kid", "parent", "", 403)
	check(admin, httpclient.GetPolicy, "admin", "other", "", 403)
	// Concurrent same-revision writes preserve the atomic compare-and-swap.
	next := strings.Replace(update, `"expected_revision":0`, `"expected_revision":1`, 1)
	results := make(chan int, 2)
	failures := make(chan error, 2)
	for range 2 {
		go func() {
			status, _, err := request(admin, httpclient.PutPolicy, "admin", "kid", next)
			results <- status
			failures <- err
		}()
	}
	a, b := <-results, <-results
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
	if (a != 200 || b != 409) && (a != 409 || b != 200) {
		t.Fatalf("revision race statuses %d %d", a, b)
	}
	check(media, httpclient.PutUserdata, "kid", "kid", `{"favorites":{"fixture":{"title":"Local fixture"}}}`, 200)
	blob := check(media, httpclient.GetUserdata, "kid", "kid", "", 200)
	if !strings.Contains(string(blob), "Local fixture") {
		t.Fatalf("blob not persisted: %s", blob)
	}
	check(media, httpclient.GetUserdata, "kid", "parent", "", 403)
	check(admin, httpclient.GetUserdata, "admin", "kid", "", 200)
	identity.mu.Lock()
	demoted := identity.identities["admin"]
	demoted.Roles = []string{"user"}
	identity.identities["admin"] = demoted
	delete(identity.identities, "kid")
	identity.mu.Unlock()
	check(admin, httpclient.PutPolicy, "admin", "kid", next, 403)
	check(media, httpclient.GetPolicy, "kid", "kid", "", 401)
	check(media, httpclient.GetUserdata, "kid", "kid", "", 401)
	before = identity.calls.Load()
	check(self, httpclient.GetHealth, "", "", "", 200)
	if b := check(self, httpclient.HeadHealth, "", "", "", 200); len(b) != 0 {
		t.Fatal("HEAD body")
	}
	if identity.calls.Load() != before {
		t.Fatal("health performed user auth")
	}
	// Stop/reopen the same database: transport does not reset revisions/blobs.
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	m = NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: db, AuthProvider: identity, ErasureDialer: noProviderDialer()})
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	doc, err = m.store.GetParentalPolicy(ctx, parental.Scope{UserID: "kid", TenantID: "home"})
	if err != nil || doc.Revision != 2 {
		t.Fatalf("revision lost on restart: %+v %v", doc, err)
	}
	persisted, revision, err := m.store.Get("kid")
	if err != nil || revision != 1 || persisted.Favorites["fixture"].Title != "Local fixture" {
		t.Fatalf("blob lost on restart: %+v %d %v", persisted, revision, err)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}
func requireBindable(t *testing.T, addr string) {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener leaked: %v", err)
	}
	_ = lis.Close()
}

func TestTransportStartupValidationAndCleanup(t *testing.T) {
	ca := testtls.NewCA(t)
	provider := ca.Issue(t, "userdata-local")
	transportEnv(t, ca, provider)
	for _, test := range []struct {
		name   string
		change func(*testing.T)
	}{
		{"missing HTTP CA", func(t *testing.T) { t.Setenv("MUXCORE_TLS_CA", "") }},
		{"malformed gRPC override", func(t *testing.T) { t.Setenv("USERDATA_TLS_CERT", "missing"); t.Setenv("USERDATA_TLS_KEY", "missing") }},
		{"unknown profile", func(t *testing.T) { t.Setenv("MUXCORE_PROFILE", "typo") }},
		{"insecure household", func(t *testing.T) { t.Setenv("MUXCORE_INSECURE_DISABLE_TLS", "1") }},
		{"legacy insecure household", func(t *testing.T) { t.Setenv("MUXCORE_DEV_TLS_SKIP", "true") }},
		{"foreign module ID override", func(t *testing.T) { t.Setenv("MUXCORE_MODULE_ID", "renamed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.change(t)
			grpcAddr, httpAddr := freeAddress(t), freeAddress(t)
			db := filepath.Join(t.TempDir(), "userdata.db")
			m := NewModule(Config{GRPCAddr: grpcAddr, HTTPAddr: httpAddr, DBPath: db, AuthProvider: freshTransportAuth(), ErasureDialer: noProviderDialer()})
			if err := m.Init(context.Background()); err == nil {
				_ = m.Stop(context.Background())
				t.Fatal("invalid config initialized")
			}
			requireBindable(t, grpcAddr)
			requireBindable(t, httpAddr)
			if _, err := os.Stat(db); !os.IsNotExist(err) {
				t.Fatalf("invalid transport opened storage: %v", err)
			}
		})
	}
	t.Run("HTTP bind failure releases gRPC and storage", func(t *testing.T) {
		busy, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer busy.Close()
		grpcAddr := freeAddress(t)
		m := NewModule(Config{GRPCAddr: grpcAddr, HTTPAddr: busy.Addr().String(), DBPath: filepath.Join(t.TempDir(), "userdata.db"), AuthProvider: freshTransportAuth(), ErasureDialer: noProviderDialer()})
		if err := m.Init(context.Background()); err == nil {
			t.Fatal("busy listener initialized")
		}
		if m.store != nil || m.grpcLis != nil {
			t.Fatal("partial Init resources retained")
		}
		requireBindable(t, grpcAddr)
	})
	t.Run("immediate start stop", func(t *testing.T) {
		for i := range 10 {
			m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), fmt.Sprint(i)+".db"), AuthProvider: freshTransportAuth(), ErasureDialer: noProviderDialer()})
			if err := m.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			grpcAddr, httpAddr := m.grpcLis.Addr().String(), m.httpLis.Addr().String()
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := m.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			requireBindable(t, grpcAddr)
			requireBindable(t, httpAddr)
		}
	})
}

// Spellings that core and the SDK do not read as insecure leave core in
// household and the SDK enrolling over mTLS, so the HTTP listener must serve TLS
// with module admission (ADR-0033 section 3), even with the profile unset.
func TestNonCoreInsecureSpellingsNeverServePlaintextHTTP(t *testing.T) {
	for _, test := range []struct{ name, value string }{
		{"MUXCORE_GRPC_INSECURE", "1"}, {"MUXCORE_GRPC_INSECURE", "true"},
		{"MUXCORE_INSECURE_DISABLE_TLS", "TRUE"}, {"MUXCORE_DEV_TLS_SKIP", "True"},
	} {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			ca := testtls.NewCA(t)
			transportEnv(t, ca, ca.Issue(t, "userdata-local"))
			t.Setenv("MUXCORE_PROFILE", "")
			t.Setenv(test.name, test.value)
			m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), "userdata.db"), AuthProvider: freshTransportAuth(), ErasureDialer: noProviderDialer()})
			ctx := context.Background()
			if err := m.Init(ctx); err != nil {
				t.Fatal(err)
			}
			if m.httpTLS == nil {
				t.Fatal("HTTP listener is plaintext")
			}
			if err := m.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Stop(ctx) })
			addr := m.httpLis.Addr().String()
			req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/health", nil)
			if err != nil {
				t.Fatal(err)
			}
			plain := &http.Client{Transport: &http.Transport{Proxy: nil}}
			defer plain.CloseIdleConnections()
			if resp, err := plain.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
					t.Fatalf("plaintext request reached the handler: %d", resp.StatusCode)
				}
			}
			id := ca.Issue(t, "admin-ui")
			c, err := httpclient.New(httpclient.Config{Origin: "https://" + addr, ModuleID: "admin-ui", Profile: "household", CertFile: id.CertFile, KeyFile: id.KeyFile, CAFile: ca.File})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseIdleConnections()
			resp, err := c.Do(ctx, httpclient.GetHealth, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("health status %d", resp.StatusCode)
			}
		})
	}
}

// The provider refuses (413) a PUT whose merge with the stored blob would exceed
// what the checked client can read back, instead of persisting an unreadable blob.
func TestMergedBlobBoundaryIsReadableThroughClient(t *testing.T) {
	ca := testtls.NewCA(t)
	transportEnv(t, ca, ca.Issue(t, "userdata-local"))
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), "userdata.db"), AuthProvider: freshTransportAuth(), ErasureDialer: noProviderDialer()})
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(ctx) })
	id := ca.Issue(t, "media-ui")
	c, err := httpclient.New(httpclient.Config{Origin: "https://" + m.httpLis.Addr().String(), ModuleID: "media-ui", Profile: "household", CertFile: id.CertFile, KeyFile: id.KeyFile, CAFile: ca.File, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	do := func(op httpclient.Operation, body string) (int, int) {
		t.Helper()
		headers := http.Header{"Authorization": {"Bearer kid"}, "X-Muxcore-User-Id": {"kid"}, "Content-Type": {"application/json"}}
		resp, err := c.Do(ctx, op, headers, strings.NewReader(body))
		if err != nil {
			t.Fatalf("operation %v: %v", op, err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, len(data)
	}
	favorite := func(id string, pad int) string {
		return `{"favorites":{"` + id + `":{"id":"` + id + `","kind":"movie","title":"` + strings.Repeat("a", pad) + `","href":"/m"}}}`
	}
	// Each PUT body stays under the 4 MiB request cap; their merge grows toward 8 MiB.
	const pad = 3<<20 + 512<<10
	if status, _ := do(httpclient.PutUserdata, favorite("one", pad)); status != 200 {
		t.Fatalf("first PUT status %d", status)
	}
	status, size := do(httpclient.PutUserdata, favorite("two", pad))
	if status != 200 || size > httpclient.MaxResponseBytes {
		t.Fatalf("second PUT status=%d size=%d", status, size)
	}
	if status, size := do(httpclient.GetUserdata, ""); status != 200 || size < 2*pad || size > httpclient.MaxResponseBytes {
		t.Fatalf("merged blob unreadable through client: status=%d size=%d", status, size)
	}
	if status, _ := do(httpclient.PutUserdata, favorite("three", pad)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize merge status %d, want 413", status)
	}
	if status, size := do(httpclient.GetUserdata, ""); status != 200 || size > httpclient.MaxResponseBytes {
		t.Fatalf("refused merge corrupted the stored blob: status=%d size=%d", status, size)
	}
}

func TestExplicitDevHTTPRetainsUserAuthorization(t *testing.T) {
	ca := testtls.NewCA(t)
	transportEnv(t, ca, ca.Issue(t, "userdata-local"))
	t.Setenv("MUXCORE_PROFILE", "dev")
	t.Setenv("MUXCORE_INSECURE_DISABLE_TLS", "true")
	t.Setenv("MUXCORE_TLS_CERT", "")
	t.Setenv("MUXCORE_TLS_KEY", "")
	t.Setenv("MUXCORE_TLS_CA", "")
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), "userdata.db"), AuthProvider: freshTransportAuth(), ErasureDialer: noProviderDialer()})
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(ctx) })
	c, err := httpclient.New(httpclient.Config{Origin: "http://" + m.httpLis.Addr().String(), Profile: "dev", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	for _, op := range []httpclient.Operation{httpclient.GetPolicy, httpclient.GetUserdata} {
		for _, token := range []string{"", "kid"} {
			headers := http.Header{"X-Muxcore-User-Id": {"kid"}}
			want := 401
			if token != "" {
				headers.Set("Authorization", "Bearer "+token)
				want = 200
			}
			resp, err := c.Do(ctx, op, headers, nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != want {
				t.Fatalf("dev operation %v token present=%v status=%d", op, token != "", resp.StatusCode)
			}
		}
	}
}

// noProviderDialer satisfies the household requirement for the ADR-0035
// reconciler in transport tests that have no identity provider to discover:
// every sweep fails closed with "no identity provider" and erases nothing.
func noProviderDialer() *erasure.ProviderDialer {
	return &erasure.ProviderDialer{Discovery: erasuretest.NewDiscovery()}
}

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Muxcore-Media/core/pkg/contracts"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/server"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	"github.com/Muxcore-Media/userdata-local/parental"
)

type policyHTTPProvider struct {
	*auth.StaticProvider
	identities  map[string]auth.PolicyIdentity
	accounts    map[string]parental.Scope
	unavailable bool
}

func (p *policyHTTPProvider) ValidatePolicyIdentity(_ context.Context, token string) (auth.PolicyIdentity, error) {
	if p.unavailable {
		return auth.PolicyIdentity{}, auth.ErrPolicyUnavailable
	}
	identity, ok := p.identities[token]
	if !ok {
		return auth.PolicyIdentity{}, auth.ErrPolicyUnauthenticated
	}
	return identity, nil
}

func (p *policyHTTPProvider) FindPolicyAccount(_ context.Context, _, id string) (parental.Scope, error) {
	account, ok := p.accounts[id]
	if !ok {
		return parental.Scope{}, auth.ErrPolicyTargetNotFound
	}
	return account, nil
}

func policyHTTPFixture(t *testing.T) (http.Handler, *policyHTTPProvider, *store.Store) {
	t.Helper()
	provider := &policyHTTPProvider{
		StaticProvider: auth.NewStaticProvider(map[string]contracts.Session{"kid": {UserID: "kid", Roles: []string{"user"}}}),
		identities: map[string]auth.PolicyIdentity{
			"admin":   {Scope: parental.Scope{UserID: "parent", TenantID: "a"}, Roles: []string{"admin"}},
			"kid":     {Scope: parental.Scope{UserID: "kid", TenantID: "a"}, Roles: []string{"user"}},
			"manager": {Scope: parental.Scope{UserID: "manager", TenantID: "a"}, Roles: []string{"manager"}},
		},
		accounts: map[string]parental.Scope{"kid": {UserID: "kid", TenantID: "a"}, "other": {UserID: "other", TenantID: "b"}},
	}
	st, err := store.New(filepath.Join(t.TempDir(), "userdata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mux := http.NewServeMux()
	server.New(st, auth.NewGuard(provider)).RegisterRoutes(mux)
	return mux, provider, st
}

func policyRequest(t *testing.T, mux http.Handler, method, token, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/parental-policy", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set(auth.UserIDHeader, target)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "attacker")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("policy response could be cached")
	}
	return w
}

const unrestrictedBody = `{"expected_revision":0,"policy":{"version":1,"mode":"unrestricted","rules":null}}`
const restrictedBody = `{"expected_revision":0,"policy":{"version":1,"mode":"restricted","rules":{"kids_mode":true,"max_rating":"PG","blocked_tags":["Horror"],"allowed_tags":[],"allow_unrated":false}}}`

func TestPolicyHTTPAuthorityLifecycle(t *testing.T) {
	mux, provider, _ := policyHTTPFixture(t)
	read := policyRequest(t, mux, http.MethodGet, "kid", "kid", "")
	var absent parental.Document
	if err := json.Unmarshal(read.Body.Bytes(), &absent); err != nil || read.Code != 200 || absent.State != "unconfigured" || absent.Policy != nil || absent.TenantID != "a" {
		t.Fatalf("absence: %d %s %v", read.Code, read.Body.String(), err)
	}
	created := policyRequest(t, mux, http.MethodPut, "admin", "kid", restrictedBody)
	if created.Code != http.StatusOK {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	stale := policyRequest(t, mux, http.MethodPut, "admin", "kid", unrestrictedBody)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale: %d %s", stale.Code, stale.Body.String())
	}
	// The unchanged legacy blob endpoint remains writable, without authority.
	req := httptest.NewRequest(http.MethodPut, "/api/userdata", strings.NewReader(`{"prefs":{"parental":{"max_parental_rating":"R","pin_hash":"legacy-secret"}}}`))
	req.Header.Set("Authorization", "Bearer kid")
	req.Header.Set(auth.UserIDHeader, "kid")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy regression: %d %s", w.Code, w.Body.String())
	}
	read = policyRequest(t, mux, http.MethodGet, "kid", "kid", "")
	var document parental.Document
	if err := json.Unmarshal(read.Body.Bytes(), &document); err != nil || document.Revision != 1 || document.Policy == nil || document.Policy.Rules.MaxRating != "PG" || bytes.Contains(read.Body.Bytes(), []byte("legacy-secret")) || bytes.Contains(read.Body.Bytes(), []byte("updated_by")) {
		t.Fatalf("policy replaced or secrets exposed: %s %v", read.Body.String(), err)
	}
	// Revoking a token or changing current roles affects the very next call.
	admin := provider.identities["admin"]
	admin.Roles = []string{"user"}
	provider.identities["admin"] = admin
	if got := policyRequest(t, mux, http.MethodPut, "admin", "kid", strings.Replace(unrestrictedBody, `"expected_revision":0`, `"expected_revision":1`, 1)); got.Code != http.StatusForbidden {
		t.Fatalf("demoted admin: %d", got.Code)
	}
	delete(provider.identities, "kid")
	if got := policyRequest(t, mux, http.MethodGet, "kid", "kid", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d", got.Code)
	}
}

func TestPolicyHTTPNegativeBoundary(t *testing.T) {
	mux, provider, st := policyHTTPFixture(t)
	for _, tc := range []struct {
		name, method, token, target, body string
		status                            int
	}{
		{"missing token", "GET", "", "kid", "", 401},
		{"invalid token", "GET", "expired", "kid", "", 401},
		{"child write", "PUT", "kid", "kid", unrestrictedBody, 403},
		{"manager write", "PUT", "manager", "kid", unrestrictedBody, 403},
		{"child other read", "GET", "kid", "parent", "", 403},
		{"cross tenant read", "GET", "admin", "other", "", 403},
		{"cross tenant write", "PUT", "admin", "other", unrestrictedBody, 403},
		{"unknown account", "GET", "admin", "absent", "", 404},
		{"missing account", "GET", "admin", "", "", 400},
		{"bad schema", "PUT", "admin", "kid", `{}`, 400},
		{"PIN hash", "PUT", "admin", "kid", strings.Replace(unrestrictedBody, `"version":1`, `"pin_hash":"secret","version":1`, 1), 400},
		{"oversized", "PUT", "admin", "kid", strings.Repeat(" ", parental.MaxBodyBytes+1), 413},
		{"no delete", "DELETE", "admin", "kid", "", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := policyRequest(t, mux, tc.method, tc.token, tc.target, tc.body)
			if got.Code != tc.status || strings.Contains(got.Body.String(), "secret") {
				t.Fatalf("got %d %s want %d", got.Code, got.Body.String(), tc.status)
			}
		})
	}
	provider.unavailable = true
	if got := policyRequest(t, mux, "GET", "kid", "kid", ""); got.Code != 503 || strings.Contains(got.Body.String(), "unconfigured") {
		t.Fatalf("auth outage: %d %s", got.Code, got.Body.String())
	}
	provider.unavailable = false
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got := policyRequest(t, mux, "GET", "kid", "kid", ""); got.Code != 503 || strings.Contains(got.Body.String(), "unconfigured") {
		t.Fatalf("store outage: %d %s", got.Code, got.Body.String())
	}
}

func TestPolicyHTTPIdentityHeadersAndTenantChanges(t *testing.T) {
	mux, provider, _ := policyHTTPFixture(t)
	for _, header := range []string{"x-auth-token", "Cookie", "X-Caller-Id"} {
		req := httptest.NewRequest(http.MethodGet, "/api/parental-policy", nil).WithContext(auth.VerifiedMeshContext("muxcore"))
		req.Header.Set(header, "admin")
		req.Header.Set(auth.UserIDHeader, "kid")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatalf("header %s bypassed bearer: %d", header, w.Code)
		}
	}
	if got := policyRequest(t, mux, "PUT", "admin", "kid", unrestrictedBody); got.Code != 200 {
		t.Fatalf("create: %d %s", got.Code, got.Body.String())
	}
	child := provider.identities["kid"]
	child.TenantID = "b"
	provider.identities["kid"] = child
	provider.accounts["kid"] = child.Scope
	if got := policyRequest(t, mux, "GET", "admin", "kid", ""); got.Code != 403 {
		t.Fatalf("admin followed child across tenants: %d", got.Code)
	}
	got := policyRequest(t, mux, "GET", "kid", "kid", "")
	var document parental.Document
	if err := json.Unmarshal(got.Body.Bytes(), &document); err != nil || document.TenantID != "b" || document.State != "unconfigured" {
		t.Fatalf("child reused former tenant policy: %s %v", got.Body.String(), err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/parental-policy?tenant_id=a", nil)
	req.Header.Set("Authorization", "Bearer kid")
	req.Header.Set(auth.UserIDHeader, "kid")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("accepted policy selector query: %d", w.Code)
	}
}

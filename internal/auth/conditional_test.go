package auth

import (
	"errors"
	"github.com/Muxcore-Media/userdata-local/conditional"
	"github.com/Muxcore-Media/userdata-local/parental"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConditionalFreshSelfHouseholdIdentity(t *testing.T) {
	provider := &policyTestProvider{StaticProvider: NewStaticProvider(nil), identity: PolicyIdentity{Scope: parental.Scope{UserID: "alice"}, Roles: []string{"admin"}}}
	guard := NewGuard(provider)
	for _, tc := range []struct {
		name, target, tenant string
		want                 error
	}{
		{"self", "alice", "", nil}, {"admin other", "bob", "", conditional.Forbidden}, {"not trimmed", " alice", "", conditional.Forbidden},
		{"case", "Alice", "", conditional.Forbidden}, {"missing", "", "", conditional.Forbidden}, {"long", strings.Repeat("a", 257), "", conditional.Forbidden},
		{"verified tenant", "alice", "household", conditional.ScopeUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider.identity.TenantID = tc.tenant
			req := httptest.NewRequest("GET", conditional.Path, nil)
			req.Header.Set("Authorization", "Bearer current")
			req.Header.Set(UserIDHeader, tc.target)
			req.Header.Set("X-Tenant-ID", "")
			if _, err := guard.AuthorizeConditionalHTTP(req); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	if provider.reads != 7 || provider.lookups != 0 {
		t.Fatalf("reads=%d lookups=%d", provider.reads, provider.lookups)
	}
	provider.identity.TenantID = ""
	for _, tc := range []struct{ source, want error }{{ErrPolicyUnauthenticated, conditional.Unauthenticated}, {ErrPolicyForbidden, conditional.Forbidden}, {ErrPolicyUnavailable, conditional.IdentityUnavailable}} {
		provider.err = tc.source
		req := httptest.NewRequest("PUT", conditional.Path, nil)
		req.Header.Set("Authorization", "Bearer current")
		req.Header.Set(UserIDHeader, "alice")
		if _, err := guard.AuthorizeConditionalHTTP(req); !errors.Is(err, tc.want) {
			t.Fatalf("error mapping: %v", err)
		}
	}
}

func TestConditionalRejectsFallbackAndDuplicateHeaders(t *testing.T) {
	provider := &policyTestProvider{StaticProvider: NewStaticProvider(nil), identity: PolicyIdentity{Scope: parental.Scope{UserID: "alice"}}}
	for _, variant := range []string{"fallback", "duplicate auth", "duplicate target", "unsupported"} {
		req := httptest.NewRequest("GET", conditional.Path, nil)
		req.Header.Set(UserIDHeader, "alice")
		req.Header.Set("X-Auth-Token", "legacy")
		guard := NewGuard(provider)
		want := conditional.Unauthenticated
		if variant != "fallback" {
			req.Header.Set("Authorization", "Bearer current")
		}
		switch variant {
		case "duplicate auth":
			req.Header.Add("Authorization", "Bearer current")
		case "duplicate target":
			req.Header.Add(UserIDHeader, "alice")
			want = conditional.Forbidden
		case "unsupported":
			guard = NewGuard(NewStaticProvider(nil))
			want = conditional.IdentityUnavailable
		}
		if _, err := guard.AuthorizeConditionalHTTP(req); !errors.Is(err, want) {
			t.Fatalf("%s: %v", variant, err)
		}
	}
}

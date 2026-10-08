package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Muxcore-Media/core/pkg/contracts"
	authv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/auth/v1"
	"github.com/Muxcore-Media/userdata-local/parental"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type policyTestProvider struct {
	*StaticProvider
	identity PolicyIdentity
	target   parental.Scope
	err      error
	reads    int
	lookups  int
}

func (p *policyTestProvider) ValidatePolicyIdentity(context.Context, string) (PolicyIdentity, error) {
	p.reads++
	return p.identity, p.err
}

func (p *policyTestProvider) FindPolicyAccount(context.Context, string, string) (parental.Scope, error) {
	p.lookups++
	if p.target.UserID == "" {
		return parental.Scope{}, ErrPolicyTargetNotFound
	}
	return p.target, nil
}

func TestPolicyAuthorizationUsesFreshIdentityAndTenant(t *testing.T) {
	provider := &policyTestProvider{StaticProvider: NewStaticProvider(nil)}
	guard := NewGuard(provider)
	cases := []struct {
		name, caller, tenant, target, targetTenant, role string
		write                                            bool
		want                                             error
	}{
		{"self read", "kid", "a", "kid", "a", "user", false, nil},
		{"self write", "kid", "a", "kid", "a", "user", true, ErrPolicyForbidden},
		{"manager write", "manager", "a", "kid", "a", "manager", true, ErrPolicyForbidden},
		{"manager other read", "manager", "a", "kid", "a", "manager", false, ErrPolicyForbidden},
		{"admin write", "parent", "a", "kid", "a", "admin", true, nil},
		{"admin self write", "parent", "a", "parent", "a", "admin", true, nil},
		{"admin other read", "parent", "a", "kid", "a", "admin", false, nil},
		{"cross tenant write", "parent", "a", "kid", "b", "admin", true, ErrPolicyForbidden},
		{"cross tenant read", "parent", "a", "kid", "b", "admin", false, ErrPolicyForbidden},
		{"empty tenant not wildcard", "parent", "", "kid", "b", "admin", true, ErrPolicyForbidden},
		{"single household", "parent", "", "kid", "", "admin", true, nil},
		{"other user", "kid", "a", "parent", "a", "user", false, ErrPolicyForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider.identity = PolicyIdentity{Scope: parental.Scope{UserID: tc.caller, TenantID: tc.tenant}, Roles: []string{tc.role}}
			provider.target = parental.Scope{UserID: tc.target, TenantID: tc.targetTenant}
			req := httptest.NewRequest(http.MethodGet, "/api/parental-policy", nil)
			req.Header.Set("Authorization", "Bearer token")
			req.Header.Set(UserIDHeader, tc.target)
			req.Header.Set("X-Tenant-ID", "spoof")
			req.Header.Set("X-Caller-Id", "parent")
			before := provider.reads
			scope, actor, err := guard.AuthorizePolicyHTTP(req, tc.write)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if provider.reads != before+1 {
				t.Fatal("policy request reused an earlier identity")
			}
			if err == nil && (scope.UserID != tc.target || scope.TenantID != tc.tenant || actor != tc.caller) {
				t.Fatalf("scope=%+v actor=%q", scope, actor)
			}
		})
	}
	provider.err = ErrPolicyUnauthenticated
	req := httptest.NewRequest(http.MethodPut, "/api/parental-policy", nil)
	req.Header.Set("Authorization", "Bearer formerly-valid")
	req.Header.Set(UserIDHeader, "kid")
	if _, _, err := guard.AuthorizePolicyHTTP(req, true); !errors.Is(err, ErrPolicyUnauthenticated) {
		t.Fatalf("revoked token: %v", err)
	}
}

func TestPolicyAuthorizationNeverUsesMeshOrLegacyFallback(t *testing.T) {
	legacy := NewStaticProvider(map[string]contracts.Session{"token": {UserID: "admin", Roles: []string{"admin"}}})
	guard := NewGuard(legacy)
	for _, header := range []string{"", "x-auth-token", "Cookie", "X-Caller-Id"} {
		req := httptest.NewRequest(http.MethodPut, "/api/parental-policy", nil).WithContext(VerifiedMeshContext("muxcore"))
		if header != "" {
			req.Header.Set(header, "token")
		}
		req.Header.Set(UserIDHeader, "admin")
		if _, _, err := guard.AuthorizePolicyHTTP(req, true); !errors.Is(err, ErrPolicyUnauthenticated) {
			t.Fatalf("fallback %q: %v", header, err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/parental-policy", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set(UserIDHeader, "admin")
	if _, _, err := guard.AuthorizePolicyHTTP(req, false); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("legacy provider without verified tenant: %v", err)
	}
	provider := &policyTestProvider{StaticProvider: legacy, identity: PolicyIdentity{Scope: parental.Scope{UserID: "admin"}, Roles: []string{"admin"}}}
	guard = NewGuard(provider)
	req.Header.Set(UserIDHeader, "absent")
	if _, _, err := guard.AuthorizePolicyHTTP(req, true); !errors.Is(err, ErrPolicyTargetNotFound) {
		t.Fatalf("absent account: %v", err)
	}
	provider.err = ErrPolicyUnavailable
	if _, _, err := guard.AuthorizePolicyHTTP(req, true); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("identity outage: %v", err)
	}
}

type policyAuthClient struct {
	authv1.AuthServiceClient
	valid *authv1.ValidateResponse
	users *authv1.ListUsersResponse
	err   error
	token string
}

func (c *policyAuthClient) Validate(_ context.Context, req *authv1.ValidateRequest, _ ...grpc.CallOption) (*authv1.ValidateResponse, error) {
	c.token = req.GetToken()
	return c.valid, c.err
}

func (c *policyAuthClient) ListUsers(ctx context.Context, _ *authv1.ListUsersRequest, _ ...grpc.CallOption) (*authv1.ListUsersResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	c.token = ""
	if values := md.Get(AuthTokenMetadataKey); len(values) == 1 {
		c.token = values[0]
	}
	return c.users, c.err
}

func TestSidecarPolicyIdentityPreservesVerifiedTenantAndBearer(t *testing.T) {
	client := &policyAuthClient{
		valid: &authv1.ValidateResponse{Valid: true, UserId: "parent", TenantId: "verified", Roles: []string{"admin"}},
		users: &authv1.ListUsersResponse{Users: []*authv1.UserInfo{{Id: "kid", TenantId: "verified"}}},
	}
	provider := &SidecarAuthProvider{client: client}
	identity, err := provider.ValidatePolicyIdentity(context.Background(), "current")
	if err != nil || identity.TenantID != "verified" || identity.UserID != "parent" || client.token != "current" {
		t.Fatalf("identity: %+v %v", identity, err)
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(AuthTokenMetadataKey, "spoof", "x-caller-id", "spoof"))
	account, err := provider.FindPolicyAccount(ctx, "current", "kid")
	if err != nil || account.TenantID != "verified" || client.token != "current" {
		t.Fatalf("account: %+v %v", account, err)
	}
	client.valid.Valid = false
	if _, err := provider.ValidatePolicyIdentity(ctx, "revoked"); !errors.Is(err, ErrPolicyUnauthenticated) {
		t.Fatalf("invalid session: %v", err)
	}
	client.err = errors.New("offline")
	if _, err := provider.ValidatePolicyIdentity(ctx, "current"); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("auth failure: %v", err)
	}
}

func TestSidecarPolicyAuthErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		want error
	}{
		{codes.Unauthenticated, ErrPolicyUnauthenticated},
		{codes.PermissionDenied, ErrPolicyForbidden},
		{codes.Unavailable, ErrPolicyUnavailable},
		{codes.Unimplemented, ErrPolicyUnavailable},
		{codes.Internal, ErrPolicyUnavailable},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			provider := &SidecarAuthProvider{client: &policyAuthClient{err: status.Error(tc.code, "provider detail must not leak")}}
			if _, err := provider.ValidatePolicyIdentity(context.Background(), "current"); !errors.Is(err, tc.want) {
				t.Fatalf("Validate: %v want %v", err, tc.want)
			}
			if _, err := provider.FindPolicyAccount(context.Background(), "current", "kid"); !errors.Is(err, tc.want) {
				t.Fatalf("ListUsers: %v want %v", err, tc.want)
			}
		})
	}
}

package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	authv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/auth/v1"
	"github.com/Muxcore-Media/userdata-local/parental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	ErrPolicyUnauthenticated = errors.New("parental policy authentication required")
	ErrPolicyForbidden       = errors.New("parental policy access forbidden")
	ErrPolicyTargetNotFound  = errors.New("parental policy account not found")
	ErrPolicyUnavailable     = errors.New("parental policy identity unavailable")
	ErrPolicyInvalidTarget   = errors.New("invalid parental policy account")
)

// PolicyIdentity preserves the authenticated tenant which contracts.Session
// does not carry. Roles must be loaded from current auth state for every request.
type PolicyIdentity struct {
	parental.Scope
	Roles []string
}

// PolicyIdentityProvider is intentionally separate from legacy userdata auth.
// Implementations must validate a current bearer, never certificate identity or
// request headers; unsupported providers must not fabricate a household tenant.
type PolicyIdentityProvider interface {
	ValidatePolicyIdentity(context.Context, string) (PolicyIdentity, error)
	FindPolicyAccount(context.Context, string, string) (parental.Scope, error)
}

// AuthorizePolicyHTTP has no mesh-certificate or x-auth-token fallback. Scope
// comes exclusively from auth; the user header is just an untrusted selector.
func (g *Guard) AuthorizePolicyHTTP(r *http.Request, write bool) (parental.Scope, string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return parental.Scope{}, "", ErrPolicyUnauthenticated
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return parental.Scope{}, "", ErrPolicyUnauthenticated
	}
	if g == nil {
		return parental.Scope{}, "", ErrPolicyUnavailable
	}
	provider, ok := g.provider.(PolicyIdentityProvider)
	if !ok {
		return parental.Scope{}, "", ErrPolicyUnavailable
	}
	token := parts[1]
	identity, err := provider.ValidatePolicyIdentity(r.Context(), token)
	if err != nil {
		return parental.Scope{}, "", err
	}
	if parental.ValidateScope(identity.Scope) != nil {
		return parental.Scope{}, "", ErrPolicyUnavailable
	}
	admin := hasRole(identity.Roles, "admin")
	if write && !admin {
		return parental.Scope{}, "", ErrPolicyForbidden
	}
	targets := r.Header.Values(UserIDHeader)
	if len(targets) != 1 || parental.ValidateScope(parental.Scope{UserID: targets[0]}) != nil {
		return parental.Scope{}, "", ErrPolicyInvalidTarget
	}
	target := targets[0]
	if target == identity.UserID {
		return identity.Scope, identity.UserID, nil
	}
	if !admin {
		return parental.Scope{}, "", ErrPolicyForbidden
	}
	subject, err := provider.FindPolicyAccount(r.Context(), token, target)
	if err != nil {
		return parental.Scope{}, "", err
	}
	if parental.ValidateScope(subject) != nil || subject.UserID != target {
		return parental.Scope{}, "", ErrPolicyUnavailable
	}
	if subject.TenantID != identity.TenantID {
		return parental.Scope{}, "", ErrPolicyForbidden
	}
	return subject, identity.UserID, nil
}

func (s *SidecarAuthProvider) ValidatePolicyIdentity(ctx context.Context, token string) (PolicyIdentity, error) {
	resp, err := s.client.Validate(ctx, &authv1.ValidateRequest{Token: token})
	if err != nil {
		return PolicyIdentity{}, policyAuthError(err)
	}
	if resp == nil {
		return PolicyIdentity{}, ErrPolicyUnavailable
	}
	if !resp.GetValid() || resp.GetUserId() == "" {
		return PolicyIdentity{}, ErrPolicyUnauthenticated
	}
	return PolicyIdentity{
		Scope: parental.Scope{UserID: resp.GetUserId(), TenantID: resp.GetTenantId()},
		Roles: append([]string(nil), resp.GetRoles()...),
	}, nil
}

func (s *SidecarAuthProvider) FindPolicyAccount(ctx context.Context, token, userID string) (parental.Scope, error) {
	// Replace rather than append outgoing identity metadata. This call occurs
	// only after a fresh admin bearer validation in AuthorizePolicyHTTP.
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs(AuthTokenMetadataKey, token))
	resp, err := s.client.ListUsers(ctx, &authv1.ListUsersRequest{})
	if err != nil {
		return parental.Scope{}, policyAuthError(err)
	}
	if resp == nil {
		return parental.Scope{}, ErrPolicyUnavailable
	}
	var found *parental.Scope
	for _, user := range resp.GetUsers() {
		if user.GetId() != userID {
			continue
		}
		if found != nil {
			return parental.Scope{}, ErrPolicyUnavailable
		}
		found = &parental.Scope{UserID: user.GetId(), TenantID: user.GetTenantId()}
	}
	if found == nil {
		return parental.Scope{}, ErrPolicyTargetNotFound
	}
	return *found, nil
}

func policyAuthError(err error) error {
	switch status.Code(err) {
	case codes.Unauthenticated:
		return ErrPolicyUnauthenticated
	case codes.PermissionDenied:
		return ErrPolicyForbidden
	default:
		return ErrPolicyUnavailable
	}
}

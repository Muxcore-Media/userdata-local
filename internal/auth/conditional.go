package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Muxcore-Media/userdata-local/conditional"
)

// AuthorizeConditionalHTTP resolves fresh self/household identity. The separate
// policy identity interface supplies the tenant that legacy Session lacks.
// Neither an administrator role nor a module certificate authorizes another user.
func (g *Guard) AuthorizeConditionalHTTP(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", conditional.Unauthenticated
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", conditional.Unauthenticated
	}
	if g == nil {
		return "", conditional.IdentityUnavailable
	}
	provider, ok := g.provider.(PolicyIdentityProvider)
	if !ok {
		return "", conditional.IdentityUnavailable
	}
	identity, err := provider.ValidatePolicyIdentity(r.Context(), parts[1])
	if err != nil {
		switch {
		case errors.Is(err, ErrPolicyUnauthenticated):
			return "", conditional.Unauthenticated
		case errors.Is(err, ErrPolicyForbidden):
			return "", conditional.Forbidden
		default:
			return "", conditional.IdentityUnavailable
		}
	}
	if !conditional.ValidID(identity.UserID) {
		return "", conditional.IdentityUnavailable
	}
	if identity.TenantID != "" {
		return "", conditional.ScopeUnsupported
	}
	targets := r.Header.Values(UserIDHeader)
	if len(targets) != 1 || !conditional.ValidID(targets[0]) || targets[0] != identity.UserID {
		return "", conditional.Forbidden
	}
	return identity.UserID, nil
}

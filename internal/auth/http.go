package auth

import (
	"net/http"
	"strings"
)

const authTokenHeaderKey = "x-auth-token"

func bearerTokenFromRequest(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if token := strings.TrimSpace(r.Header.Get(authTokenHeaderKey)); token != "" {
		return token
	}
	return ""
}

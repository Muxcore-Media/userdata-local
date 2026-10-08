package httptransport

import "net/http"

const ModuleForbiddenCode = "userdata.module_forbidden"
const AdmissionErrorHeader = "X-MuxCore-Error-Code"

// Admit wraps the entire mux so even redirects and unknown paths require an
// admitted verified module. Only the TLS verified chain establishes identity.
func Admit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.VerifiedChains[0]) > 0 {
			cn = r.TLS.VerifiedChains[0][0].Subject.CommonName
		}
		if r.URL.RawPath != "" || !allowed(cn, r.Method, r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(AdmissionErrorHeader, ModuleForbiddenCode)
			w.WriteHeader(http.StatusForbidden)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(`{"code":"` + ModuleForbiddenCode + `"}`))
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowed(cn, method, path string) bool {
	switch path {
	case "/health":
		return (method == http.MethodGet || method == http.MethodHead) &&
			(cn == "media-ui" || cn == "admin-ui" || cn == Provider || cn == "health-monitor")
	case "/api/parental-policy":
		return (cn == "media-ui" && method == http.MethodGet) ||
			(cn == "admin-ui" && (method == http.MethodGet || method == http.MethodPut))
	case "/api/userdata":
		return (cn == "media-ui" || cn == "admin-ui") && (method == http.MethodGet || method == http.MethodPut)
	default:
		return false
	}
}

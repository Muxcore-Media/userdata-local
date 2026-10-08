package httptransport

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muxcore-Media/userdata-local/internal/testtls"
)

func serverFixture(t *testing.T) (*testtls.CA, *httptest.Server, *atomic.Int64) {
	t.Helper()
	ca := testtls.NewCA(t)
	id := ca.Issue(t, Provider)
	cfg, err := ServerConfig(Config{Profile: "household", ModuleID: Provider, CertFile: id.CertFile, KeyFile: id.KeyFile, CAFile: ca.File})
	if err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int64{}
	s := httptest.NewUnstartedServer(Admit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("admitted"))
		}
	})))
	s.TLS = cfg
	s.StartTLS()
	t.Cleanup(s.Close)
	return ca, s, calls
}

func rawClient(t *testing.T, ca *testtls.CA, pair *tls.Certificate) *http.Client {
	t.Helper()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: ca.Pool, ServerName: Provider}
	if pair != nil {
		cfg.Certificates = []tls.Certificate{*pair}
	}
	transport := &http.Transport{TLSClientConfig: cfg, Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestTLSAdmissionExactTable(t *testing.T) {
	ca, s, calls := serverFixture(t)
	for _, cn := range []string{"media-ui", "admin-ui", Provider, "health-monitor", "jellyfin", "muxcore", "Admin-ui", "admin-ui.attacker", ""} {
		id := ca.Issue(t, cn)
		client := rawClient(t, ca, &id.Pair)
		for _, path := range []string{"/api/parental-policy", "/api/userdata", "/health", "/other", "/api/userdata/", "//health", "/api/%75serdata"} {
			for _, method := range []string{"GET", "HEAD", "PUT", "DELETE", "POST", "OPTIONS", "PATCH"} {
				t.Run(cn+method+path, func(t *testing.T) {
					before := calls.Load()
					req, err := http.NewRequest(method, s.URL+path, nil)
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("X-Caller-ID", "admin-ui")
					req.Header.Set("X-MuxCore-Module-ID", "admin-ui")
					req.Header.Set("Authorization", "Bearer admin")
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					// Independent expected table; never use production allowed().
					want := false
					if path == "/health" && (method == "GET" || method == "HEAD") {
						want = cn == "media-ui" || cn == "admin-ui" || cn == Provider || cn == "health-monitor"
					}
					if path == "/api/userdata" && (method == "GET" || method == "PUT") {
						want = cn == "media-ui" || cn == "admin-ui"
					}
					if path == "/api/parental-policy" {
						want = (cn == "media-ui" && method == "GET") || (cn == "admin-ui" && (method == "GET" || method == "PUT"))
					}
					if want {
						if resp.StatusCode != 200 || calls.Load() != before+1 {
							t.Fatalf("admitted request failed: %d %s", resp.StatusCode, body)
						}
					} else {
						if resp.StatusCode != 403 || calls.Load() != before {
							t.Fatalf("admission crossed handler/auth/storage boundary: %d %s", resp.StatusCode, body)
						}
						if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Type") != "application/json" {
							t.Fatal("unsafe denial headers")
						}
						if resp.Header.Get(AdmissionErrorHeader) != ModuleForbiddenCode {
							t.Fatal("missing bodyless admission discriminator")
						}
						if method != "HEAD" && string(body) != `{"code":"userdata.module_forbidden"}` {
							t.Fatalf("denial leaks details: %q", body)
						}
					}
					if method == "HEAD" && len(body) != 0 {
						t.Fatal("HEAD body")
					}
				})
			}
		}
	}
}

func TestTLSRejectsInvalidClientBeforeHandler(t *testing.T) {
	ca, s, calls := serverFixture(t)
	other := testtls.NewCA(t)
	bad := map[string]*tls.Certificate{"missing": nil}
	for name, id := range map[string]testtls.Identity{
		"untrusted": other.Issue(t, "admin-ui"),
		"expired": ca.Issue(t, "admin-ui", func(c *x509.Certificate) {
			c.NotBefore = time.Now().Add(-2 * time.Hour)
			c.NotAfter = time.Now().Add(-time.Hour)
		}),
		"wrong EKU": ca.Issue(t, "admin-ui", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} }),
	} {
		pair := id.Pair
		bad[name] = &pair
	}
	for name, pair := range bad {
		t.Run(name, func(t *testing.T) {
			resp, err := rawClient(t, ca, pair).Get(s.URL + "/api/parental-policy")
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("invalid client completed TLS handshake")
			}
			if calls.Load() != 0 {
				t.Fatal("TLS failure reached handler")
			}
		})
	}
	resp, err := http.Get(strings.Replace(s.URL, "https:", "http:", 1) + "/health")
	if resp != nil {
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("plaintext accepted")
		}
	}
	if err == nil && resp == nil {
		t.Fatal("missing plaintext result")
	}
	if calls.Load() != 0 {
		t.Fatal("plaintext reached handler")
	}
}

func TestAdmissionDoesNotTrustUnverifiedPeer(t *testing.T) {
	ca := testtls.NewCA(t)
	id := ca.Issue(t, "admin-ui")
	leaf, err := x509.ParseCertificate(id.Pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	h := Admit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unverified certificate admitted") }))
	for _, state := range []*tls.ConnectionState{nil, {PeerCertificates: []*x509.Certificate{leaf}}} {
		req := httptest.NewRequest("GET", "/api/parental-policy", nil)
		req.TLS = state
		req.Header.Set("X-Caller-ID", "admin-ui")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"MUXCORE_PROFILE", "MUXCORE_INSECURE_DISABLE_TLS", "MUXCORE_GRPC_INSECURE", "MUXCORE_DEV_TLS_SKIP", "MUXCORE_TLS_CERT", "MUXCORE_TLS_KEY", "MUXCORE_TLS_CA", "MUXCORE_TLS_DIR", "MUXCORE_CA_EXPORT_DIR"} {
		t.Setenv(name, "")
	}
}

func TestProfilesAndAliases(t *testing.T) {
	for _, profile := range []string{"household", "staging", "dev", "", "typo"} {
		for _, alias := range []string{"MUXCORE_INSECURE_DISABLE_TLS", "MUXCORE_GRPC_INSECURE", "MUXCORE_DEV_TLS_SKIP"} {
			for _, value := range []string{"true", "1", "TRUE"} {
				t.Run(profile+alias+value, func(t *testing.T) {
					cleanEnv(t)
					t.Setenv("MUXCORE_PROFILE", profile)
					t.Setenv(alias, value)
					cfg, err := FromEnv(Provider)
					wantOK := profile == "dev" || profile == ""
					if (err == nil) != wantOK {
						t.Fatalf("profile enforcement: %v", err)
					}
					if wantOK {
						tlsCfg, err := ServerConfig(cfg)
						if err != nil || tlsCfg != nil {
							t.Fatalf("dev: %v", err)
						}
					}
				})
			}
		}
	}
	cleanEnv(t)
	t.Setenv("MUXCORE_PROFILE", "dev")
	t.Setenv("MUXCORE_DEV_TLS_SKIP", "ture")
	if _, err := FromEnv(Provider); err == nil {
		t.Fatal("invalid alias value accepted")
	}
	for _, cfg := range []Config{{Profile: "household", Insecure: true}, {Profile: "staging", Insecure: true}, {Profile: "typo", Insecure: true}} {
		if _, err := ServerConfig(cfg); err == nil {
			t.Fatal("listener relied on prior SDK profile validation")
		}
	}
}

func TestExistingIdentityAndProviderConfig(t *testing.T) {
	cleanEnv(t)
	unset, err := FromEnv(Provider)
	if err != nil || unset.Insecure {
		t.Fatalf("unset profile must infer secure household: %+v %v", unset, err)
	}
	if _, err := ServerConfig(unset); err == nil {
		t.Fatal("unset profile without identity accepted")
	}
	ca := testtls.NewCA(t)
	id := ca.Issue(t, Provider)
	valid := Config{Profile: "household", ModuleID: Provider, CertFile: id.CertFile, KeyFile: id.KeyFile, CAFile: ca.File}
	tlsCfg, err := ServerConfig(valid)
	if err != nil || tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert || tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("secure config: %+v %v", tlsCfg, err)
	}
	for name, change := range map[string]func(*Config){
		"missing cert": func(c *Config) { c.CertFile = "" }, "missing key": func(c *Config) { c.KeyFile = "" }, "missing CA": func(c *Config) { c.CAFile = "" },
		"unreadable cert": func(c *Config) { c.CertFile = "missing" }, "unreadable key": func(c *Config) { c.KeyFile = "missing" }, "unreadable CA": func(c *Config) { c.CAFile = "missing" },
		"wrong provider": func(c *Config) { c.ModuleID = "admin-ui" },
		"borrowed CN": func(c *Config) {
			i := ca.Issue(t, "admin-ui", func(cert *x509.Certificate) { cert.DNSNames = []string{Provider} })
			c.CertFile = i.CertFile
			c.KeyFile = i.KeyFile
		},
		"missing SAN": func(c *Config) {
			i := ca.Issue(t, Provider, func(cert *x509.Certificate) { cert.DNSNames = []string{"localhost"} })
			c.CertFile = i.CertFile
			c.KeyFile = i.KeyFile
		},
		"expired": func(c *Config) {
			i := ca.Issue(t, Provider, func(cert *x509.Certificate) {
				cert.NotBefore = time.Now().Add(-2 * time.Hour)
				cert.NotAfter = time.Now().Add(-time.Hour)
			})
			c.CertFile = i.CertFile
			c.KeyFile = i.KeyFile
		},
		"wrong EKU": func(c *Config) {
			i := ca.Issue(t, Provider, func(cert *x509.Certificate) { cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} })
			c.CertFile = i.CertFile
			c.KeyFile = i.KeyFile
		},
		"wrong CA":              func(c *Config) { c.CAFile = testtls.NewCA(t).File },
		"mismatched key":        func(c *Config) { c.KeyFile = ca.Issue(t, Provider).KeyFile },
		"malformed certificate": func(c *Config) { c.CertFile = id.KeyFile },
		"malformed key":         func(c *Config) { c.KeyFile = id.CertFile },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			change(&cfg)
			if _, err := ServerConfig(cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	for _, data := range []string{"", "not PEM", "-----BEGIN CERTIFICATE-----\nmalformed\n-----END CERTIFICATE-----", string(mustRead(t, ca.File)) + "garbage"} {
		path := filepath.Join(t.TempDir(), "bad.crt")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		cfg := valid
		cfg.CAFile = path
		if _, err := ServerConfig(cfg); err == nil {
			t.Fatal("malformed CA accepted")
		}
	}
	// Separately invoked helpers see files in TLS_DIR, not daemon-only exports.
	t.Setenv("MUXCORE_TLS_DIR", filepath.Dir(id.CertFile))
	t.Setenv("MUXCORE_TLS_CA", ca.File)
	cfg, err := FromEnv(Provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ServerConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUXCORE_TLS_CERT", id.CertFile)
	if _, err := FromEnv(Provider); err == nil {
		t.Fatal("partial explicit paths silently replaced")
	}
	t.Setenv("MUXCORE_TLS_CERT", "")
	t.Setenv("MUXCORE_TLS_CA", "")
	t.Setenv("MUXCORE_TLS_DIR", t.TempDir())
	t.Setenv("MUXCORE_BOOTSTRAP_TOKEN", "fixture-must-not-be-consumed")
	cfg, err = FromEnv(Provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ServerConfig(cfg); err == nil {
		t.Fatal("missing identity accepted")
	}
	files, err := os.ReadDir(os.Getenv("MUXCORE_TLS_DIR"))
	if err != nil || len(files) != 0 {
		t.Fatal("helper generated identity files")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

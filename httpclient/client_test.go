package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muxcore-Media/userdata-local/internal/httptransport"
	"github.com/Muxcore-Media/userdata-local/internal/testtls"
)

func tlsServer(t *testing.T, ca *testtls.CA, id testtls.Identity, handler http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{id.Pair}, ClientCAs: ca.Pool, ClientAuth: tls.RequireAndVerifyClientCert}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func clientConfig(ca *testtls.CA, id testtls.Identity, origin, cn string) Config {
	return Config{Origin: origin, ModuleID: cn, Profile: "household", CertFile: id.CertFile, KeyFile: id.KeyFile, CAFile: ca.File}
}

func newClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func TestProviderIdentityBeforeBearer(t *testing.T) {
	ca := testtls.NewCA(t)
	other := testtls.NewCA(t)
	caller := ca.Issue(t, "media-ui")
	for name, id := range map[string]testtls.Identity{
		"valid":                           ca.Issue(t, "userdata-local"),
		"shared loopback SAN impostor":    ca.Issue(t, "admin-ui"),
		"wrong CN even with provider SAN": ca.Issue(t, "admin-ui", func(c *x509.Certificate) { c.DNSNames = []string{"userdata-local"} }),
		"wrong SAN":                       ca.Issue(t, "userdata-local", func(c *x509.Certificate) { c.DNSNames = []string{"localhost"} }),
		"wrong CA":                        other.Issue(t, "userdata-local"),
		"wrong EKU":                       ca.Issue(t, "userdata-local", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }),
		"expired": ca.Issue(t, "userdata-local", func(c *x509.Certificate) {
			c.NotBefore = time.Now().Add(-2 * time.Hour)
			c.NotAfter = time.Now().Add(-time.Hour)
		}),
		"not yet valid": ca.Issue(t, "userdata-local", func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Minute) }),
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			s := tlsServer(t, ca, id, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer fixture-secret" {
					t.Error("missing caller-provided bearer")
				}
				if r.TLS.VerifiedChains[0][0].Subject.CommonName != "media-ui" {
					t.Error("borrowed module identity")
				}
				_, _ = w.Write([]byte(`{"state":"unconfigured"}`))
			}))
			t.Setenv("MUXCORE_TLS_SERVER_NAME", "admin-ui") // Must not affect expected provider.
			c := newClient(t, clientConfig(ca, caller, s.URL, "media-ui"))
			resp, err := c.Do(context.Background(), GetPolicy, http.Header{"Authorization": {"Bearer fixture-secret"}}, nil)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if name == "valid" {
				if err != nil || calls.Load() != 1 {
					t.Fatalf("valid provider failed: %v", err)
				}
			} else {
				if !errors.Is(err, ErrUnavailable) || calls.Load() != 0 {
					t.Fatalf("bearer reached impostor: calls=%d err=%v", calls.Load(), err)
				}
				if strings.Contains(err.Error(), "fixture-secret") {
					t.Fatal("error leaked bearer")
				}
			}
		})
	}
}

func TestApplicationErrorsAndAdmissionAreDistinct(t *testing.T) {
	ca := testtls.NewCA(t)
	caller := ca.Issue(t, "media-ui")
	for _, tc := range []struct {
		name   string
		status int
		body   string
		reason string
	}{
		{"module refusal", 403, `{"code":"userdata.module_forbidden"}`, ReasonModuleForbidden},
		{"application forbidden", 403, `{"code":"policy.forbidden"}`, ""},
		{"unauthenticated", 401, `{"code":"policy.unauthenticated"}`, ""},
		{"revision conflict", 409, `{"code":"policy.revision_conflict"}`, ""},
		{"auth unavailable", 503, `{"code":"policy.identity_unavailable"}`, ""},
		{"malformed refusal", 403, `{"code":"userdata.module_forbidden"}extra`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tlsServer(t, ca, ca.Issue(t, "userdata-local"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			c := newClient(t, clientConfig(ca, caller, s.URL, "media-ui"))
			resp, err := c.Do(context.Background(), PutPolicy, nil, strings.NewReader("{}"))
			if tc.reason != "" {
				var unavailable *UnavailableError
				if !errors.Is(err, ErrUnavailable) || !errors.As(err, &unavailable) || unavailable.Reason != tc.reason || resp != nil {
					t.Fatalf("admission mapping: %v %+v", err, resp)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != tc.status || string(body) != tc.body {
					t.Fatal("application meaning changed")
				}
			}
		})
	}
	// Real admission wrapper, not just a fixture-generated denial document.
	s := tlsServer(t, ca, ca.Issue(t, "userdata-local"), httptransport.Admit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("forbidden write reached application") })))
	c := newClient(t, clientConfig(ca, caller, s.URL, "media-ui"))
	_, err := c.Do(context.Background(), PutPolicy, http.Header{"Authorization": {"Bearer admin"}}, strings.NewReader("{}"))
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != ReasonModuleForbidden {
		t.Fatalf("real admission: %v", err)
	}
	unlisted := ca.Issue(t, "jellyfin")
	unlistedClient := newClient(t, clientConfig(ca, unlisted, s.URL, "jellyfin"))
	_, err = unlistedClient.Do(context.Background(), HeadHealth, nil, nil)
	if !errors.As(err, &unavailable) || unavailable.Reason != ReasonModuleForbidden {
		t.Fatalf("bodyless HEAD admission: %v", err)
	}
}

func TestAllRedirectsAreRefusedWithoutSendingBearer(t *testing.T) {
	ca := testtls.NewCA(t)
	caller := ca.Issue(t, "admin-ui")
	provider := ca.Issue(t, "userdata-local")
	var targets atomic.Int64
	target := tlsServer(t, ca, provider, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targets.Add(1) }))
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, kind := range []string{"relative", "same origin", "cross origin", "scheme relative", "downgrade"} {
			t.Run(http.StatusText(status)+kind, func(t *testing.T) {
				var source *httptest.Server
				var sourceCalls atomic.Int64
				source = tlsServer(t, ca, provider, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if sourceCalls.Add(1) > 1 {
						targets.Add(1)
						return
					}
					if r.Method != "PUT" || r.URL.Path != "/api/parental-policy" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
						t.Error("wrong source request")
					}
					location := "/redirected"
					switch kind {
					case "same origin":
						location = source.URL + "/redirected"
					case "cross origin":
						location = target.URL + "/redirected"
					case "scheme relative":
						location = strings.TrimPrefix(target.URL, "https:") + "/redirected"
					case "downgrade":
						location = strings.Replace(target.URL, "https:", "http:", 1) + "/redirected"
					}
					w.Header().Set("Location", location)
					w.WriteHeader(status)
				}))
				c := newClient(t, clientConfig(ca, caller, source.URL, "admin-ui"))
				_, err := c.Do(context.Background(), PutPolicy, http.Header{"Authorization": {"Bearer fixture-secret"}}, strings.NewReader("{}"))
				var unavailable *UnavailableError
				if !errors.As(err, &unavailable) || unavailable.Reason != ReasonRedirect {
					t.Fatalf("redirect accepted: %v", err)
				}
				if targets.Load() != 0 {
					t.Fatal("redirect sent bearer to target")
				}
			})
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOriginBoundTransportRejectsBeforeDial(t *testing.T) {
	u, _ := url.Parse("https://userdata-local:9701")
	var calls int
	bound := &originTransport{origin: *u, next: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	for _, target := range []string{"https://external.example/api/userdata", "http://userdata-local:9701/api/userdata", "https://userdata-local:9702/api/userdata", "https://user@userdata-local:9701/api/userdata"} {
		req, err := http.NewRequest("GET", target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bound.RoundTrip(req); err == nil {
			t.Fatalf("foreign request accepted: %s", target)
		}
	}
	req, _ := http.NewRequest("GET", u.String()+"/health", nil)
	req.Host = "external.example"
	if _, err := bound.RoundTrip(req); err == nil {
		t.Fatal("host override accepted")
	}
	if calls != 0 {
		t.Fatal("foreign origin reached dialer")
	}
	req.Host = ""
	resp, err := bound.RoundTrip(req)
	if err != nil || calls != 1 {
		t.Fatal("valid origin rejected")
	}
	_ = resp.Body.Close()
}

func TestClientBoundsProxyCancellationAndOperations(t *testing.T) {
	var proxyCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	ca := testtls.NewCA(t)
	caller := ca.Issue(t, "admin-ui")
	s := tlsServer(t, ca, ca.Issue(t, "userdata-local"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("client invented credentials")
		}
		if r.Header.Get("X-Test") == "oversize" {
			_, _ = w.Write([]byte(strings.Repeat("a", MaxResponseBytes+1)))
			return
		}
		// Stalled handlers wait for the client to go away and then abort the
		// connection. Returning normally would let net/http complete a valid
		// (empty 200) response that the client can read just before its own
		// teardown lands, which is the race the deadline recheck in Do closes.
		if r.Header.Get("X-Test") == "delay" {
			<-r.Context().Done()
			panic(http.ErrAbortHandler)
		}
		if r.Header.Get("X-Test") == "body-delay" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("X-Route", r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte("ok"))
	}))
	cfg := clientConfig(ca, caller, s.URL, "admin-ui")
	c := newClient(t, cfg)
	for op, want := range map[Operation]string{GetPolicy: "GET /api/parental-policy", PutPolicy: "PUT /api/parental-policy", GetUserdata: "GET /api/userdata", PutUserdata: "PUT /api/userdata", GetHealth: "GET /health", HeadHealth: "HEAD /health"} {
		resp, err := c.Do(context.Background(), op, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.Header.Get("X-Route") != want {
			t.Fatal("operation did not fix endpoint")
		}
	}
	if _, err := c.Do(context.Background(), Operation(99), nil, nil); err == nil {
		t.Fatal("invalid operation accepted")
	}
	if proxyCalls.Load() != 0 {
		t.Fatal("mesh identity sent to proxy")
	}
	_, err := c.Do(context.Background(), GetPolicy, http.Header{"X-Test": {"oversize"}}, nil)
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != ReasonResponse {
		t.Fatalf("unbounded response: %v", err)
	}
	cfg.Timeout = 100 * time.Millisecond
	timedClient := newClient(t, cfg)
	start := time.Now()
	_, err = timedClient.Do(context.Background(), GetPolicy, http.Header{"X-Test": {"delay"}}, nil)
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > time.Second {
		t.Fatalf("unbounded request: %v", err)
	}
	bodyCtx, bodyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer bodyCancel()
	start = time.Now()
	_, err = timedClient.Do(bodyCtx, GetPolicy, http.Header{"X-Test": {"body-delay"}}, nil)
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > time.Second {
		t.Fatalf("response body escaped request deadline: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Do(ctx, GetPolicy, nil, nil)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

// lateBody delivers a complete, valid body only after its request context has
// expired, ignoring that context, exactly like a response that net/http finished
// racing the client's connection teardown.
type lateBody struct {
	ctx  context.Context
	done bool
}

func (b *lateBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	<-b.ctx.Done()
	b.done = true
	return copy(p, "late"), io.EOF
}
func (b *lateBody) Close() error { return nil }

type lateTransport struct{}

func (lateTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &lateBody{ctx: r.Context()}, Request: r}, nil
}

// TestDoRejectsResponseCompletedAfterDeadline deterministically covers the race
// where a response is fully read after the deadline fired: Do must report a
// transport timeout rather than a 200 with a (possibly empty) body.
func TestDoRejectsResponseCompletedAfterDeadline(t *testing.T) {
	ca := testtls.NewCA(t)
	cfg := clientConfig(ca, ca.Issue(t, "admin-ui"), "https://127.0.0.1:9701", "admin-ui")
	cfg.Timeout = 50 * time.Millisecond
	c := newClient(t, cfg)
	c.client.Transport = lateTransport{}
	resp, err := c.Do(context.Background(), GetUserdata, nil, nil)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response completed after the deadline was returned: %d", resp.StatusCode)
	}
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != ReasonTransport || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late response not reported as transport timeout: %v", err)
	}
	// A caller deadline earlier than the client timeout also applies.
	cfg.Timeout = 30 * time.Second
	c = newClient(t, cfg)
	c.client.Transport = lateTransport{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if resp, err := c.Do(ctx, GetUserdata, nil, nil); err == nil || !errors.Is(err, ErrUnavailable) {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatalf("late response beat caller deadline: %v", err)
	}
}

func TestInvalidClientConfiguration(t *testing.T) {
	ca := testtls.NewCA(t)
	caller := ca.Issue(t, "media-ui")
	valid := clientConfig(ca, caller, "https://127.0.0.1:9701", "media-ui")
	for _, origin := range []string{"", "http://127.0.0.1:9701", "https://user:pass@host", "https://host/", "https://host/api", "https://host?x", "https://host?", "https://host#x", "https://host#", "https://0.0.0.0:1", "https://[::]:1", "https://::1", "https://host:0", "https://host:65536", "https://host:", "ftp://host", "https:opaque"} {
		cfg := valid
		cfg.Origin = origin
		if _, err := New(cfg); err == nil {
			t.Fatalf("invalid origin accepted: %s", origin)
		}
	}
	for name, change := range map[string]func(*Config){
		"borrowed CN": func(c *Config) { c.ModuleID = "admin-ui" }, "missing identity": func(c *Config) { c.ModuleID = "" },
		"missing cert": func(c *Config) { c.CertFile = "" }, "missing key": func(c *Config) { c.KeyFile = "" }, "missing CA": func(c *Config) { c.CAFile = "" },
		"negative timeout": func(c *Config) { c.Timeout = -1 }, "excess timeout": func(c *Config) { c.Timeout = 31 * time.Second },
		"unknown profile": func(c *Config) { c.Profile = "typo" }, "insecure household": func(c *Config) { c.Insecure = true }, "insecure staging": func(c *Config) { c.Insecure = true; c.Profile = "staging" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			change(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("bad client configuration accepted")
			}
		})
	}
	for _, profile := range []string{"dev", ""} {
		c, err := New(Config{Origin: "http://127.0.0.1:9701", Profile: profile, Insecure: true})
		if err != nil {
			t.Fatal(err)
		}
		c.CloseIdleConnections()
	}
}

func TestEnvironmentProxyCannotReceiveMeshRequest(t *testing.T) {
	var proxyCalls, providerCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	ca := testtls.NewCA(t)
	caller := ca.Issue(t, "media-ui")
	provider := tlsServer(t, ca, ca.Issue(t, "userdata-local"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("provider did not receive bearer")
		}
		w.WriteHeader(http.StatusOK)
	}))
	// A logical non-loopback hostname avoids Go's implicit loopback proxy
	// bypass. Resolve only these fixture endpoints, never external DNS/network.
	const logical = "userdata-fixture.invalid:9701"
	c := newClient(t, clientConfig(ca, caller, "https://"+logical, "media-ui"))
	providerURL, _ := url.Parse(provider.URL)
	proxyURL, _ := url.Parse(proxy.URL)
	c.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		switch address {
		case logical:
			address = providerURL.Host
		case proxyURL.Host:
			// If a ProxyFromEnvironment mutation routes here, the actual proxy
			// fixture records it and rejects CONNECT.
		default:
			return nil, errors.New("test refuses non-fixture dial")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	resp, err := c.Do(context.Background(), GetPolicy, http.Header{"Authorization": {"Bearer fixture-secret"}}, nil)
	if err != nil {
		t.Fatalf("provider request was diverted: %v (proxy calls %d)", err, proxyCalls.Load())
	}
	_ = resp.Body.Close()
	if providerCalls.Load() != 1 || proxyCalls.Load() != 0 {
		t.Fatalf("provider=%d proxy=%d", providerCalls.Load(), proxyCalls.Load())
	}
}

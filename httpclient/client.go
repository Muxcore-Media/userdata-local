// Package httpclient provides the checked userdata-local HTTP client (ADR-0033).
// Callers supply headers derived from their current server session. This package
// never creates user credentials, enrolls a module or falls back after TLS errors.
package httpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Muxcore-Media/userdata-local/internal/httptransport"
)

const (
	DefaultTimeout   = 5 * time.Second
	MaxTimeout       = 30 * time.Second
	MaxResponseBytes = 8 << 20
)

// ErrUnavailable distinguishes transport/admission failure from an authenticated
// application's HTTP status (including policy.forbidden, 401, 409 and 503).
var ErrUnavailable = errors.New("userdata provider unavailable")

// UnavailableError describes transport failures without including credentials or
// response bodies. Only ReasonModuleForbidden proves a write was not applied;
// connection loss and timeouts can leave the write outcome uncertain.
type UnavailableError struct {
	Reason string
	Cause  error
}

const (
	ReasonModuleForbidden = "module_forbidden"
	ReasonTransport       = "transport"
	ReasonRedirect        = "redirect"
	ReasonResponse        = "response"
)

func (e *UnavailableError) Error() string        { return ErrUnavailable.Error() + ": " + e.Reason }
func (e *UnavailableError) Unwrap() error        { return e.Cause }
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// Config requires the calling module's own certificate (CN equals ModuleID).
// Origin is an HTTP(S) origin, with no path, credentials, query or fragment.
// Secure mode requires HTTPS and an explicit CA. Timeout defaults to 5 seconds
// and cannot exceed 30 seconds; a caller's earlier context deadline wins.
type Config struct {
	Origin   string
	ModuleID string
	Profile  string
	Insecure bool
	CertFile string
	KeyFile  string
	CAFile   string
	Timeout  time.Duration
}

// FromEnv resolves existing MUXCORE_TLS_CERT/KEY/CA or MUXCORE_TLS_DIR identity
// files and the mounted CA. It never enrolls or reads a bootstrap token.
// New validates the resulting material before any requests can be made.
func FromEnv(origin, moduleID string) (Config, error) {
	cfg, err := httptransport.FromEnv(moduleID)
	if err != nil {
		return Config{}, err
	}
	return Config{Origin: origin, ModuleID: cfg.ModuleID, Profile: cfg.Profile,
		Insecure: cfg.Insecure, CertFile: cfg.CertFile, KeyFile: cfg.KeyFile, CAFile: cfg.CAFile}, nil
}

// Operation selects an exact method and path; arbitrary URLs are not accepted.
type Operation uint8

const (
	GetPolicy Operation = iota + 1
	PutPolicy
	GetUserdata
	PutUserdata
	GetHealth
	HeadHealth
)

func (op Operation) route() (string, string, error) {
	switch op {
	case GetPolicy:
		return http.MethodGet, "/api/parental-policy", nil
	case PutPolicy:
		return http.MethodPut, "/api/parental-policy", nil
	case GetUserdata:
		return http.MethodGet, "/api/userdata", nil
	case PutUserdata:
		return http.MethodPut, "/api/userdata", nil
	case GetHealth:
		return http.MethodGet, "/health", nil
	case HeadHealth:
		return http.MethodHead, "/health", nil
	default:
		return "", "", fmt.Errorf("userdata HTTP: unsupported operation")
	}
}

// Client is safe for concurrent use. Its origin, TLS identity, redirects, proxy
// policy and deadlines cannot be overridden by requests or caller headers.
type Client struct {
	origin    *url.URL
	client    *http.Client
	transport *http.Transport
}

func New(cfg Config) (*Client, error) {
	if err := httptransport.ValidateMode(cfg.Profile, cfg.Insecure); err != nil {
		return nil, err
	}
	origin, err := parseOrigin(cfg.Origin, cfg.Insecure)
	if err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Timeout < 0 || cfg.Timeout > MaxTimeout {
		return nil, fmt.Errorf("userdata HTTP: timeout must be positive and at most 30 seconds")
	}
	var tlsConfig *tls.Config
	if !cfg.Insecure {
		pair, roots, err := httptransport.LoadIdentity(httptransport.Config{
			ModuleID: cfg.ModuleID, CertFile: cfg.CertFile, KeyFile: cfg.KeyFile, CAFile: cfg.CAFile,
		}, x509.ExtKeyUsageClientAuth)
		if err != nil {
			return nil, err
		}
		tlsConfig = &tls.Config{
			MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{pair},
			ServerName: httptransport.Provider,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 ||
					state.VerifiedChains[0][0].Subject.CommonName != httptransport.Provider {
					return fmt.Errorf("userdata HTTP: provider identity mismatch")
				}
				return nil
			},
		}
	}
	transport := &http.Transport{
		Proxy: nil, TLSClientConfig: tlsConfig,
		DialContext:         (&net.Dialer{Timeout: cfg.Timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: cfg.Timeout, ResponseHeaderTimeout: cfg.Timeout,
		IdleConnTimeout: 30 * time.Second, MaxIdleConns: 8, MaxIdleConnsPerHost: 8,
		MaxConnsPerHost: 16, MaxResponseHeaderBytes: 32 << 10,
	}
	client := &http.Client{
		Transport: &originTransport{origin: *origin, next: transport}, Timeout: cfg.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errRedirect },
	}
	return &Client{origin: origin, client: client, transport: transport}, nil
}

// CloseIdleConnections releases idle connections during shutdown/reconfiguration.
func (c *Client) CloseIdleConnections() { c.transport.CloseIdleConnections() }

var errRedirect = errors.New("userdata HTTP redirects are forbidden")

// Do executes a fixed provider operation. Headers are copied unchanged; the
// caller owns deriving the current bearer and target user. Responses are read
// completely within the deadline and bounded to MaxResponseBytes. Application
// statuses are returned for the caller to interpret. Close the response body.
func (c *Client) Do(ctx context.Context, op Operation, headers http.Header, body io.Reader) (*http.Response, error) {
	method, path, err := op.route()
	if err != nil {
		return nil, err
	}
	target := *c.origin
	target.Path = path
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	resp, err := c.client.Do(req)
	if err != nil {
		reason := ReasonTransport
		if errors.Is(err, errRedirect) {
			reason = ReasonRedirect
		}
		return nil, &UnavailableError{Reason: reason, Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &UnavailableError{Reason: ReasonRedirect}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, &UnavailableError{Reason: ReasonTransport, Cause: err}
	}
	if len(data) > MaxResponseBytes {
		return nil, &UnavailableError{Reason: ReasonResponse}
	}
	if resp.StatusCode == http.StatusForbidden && len(data) <= 256 {
		var denial struct {
			Code string `json:"code"`
		}
		if resp.Header.Get(httptransport.AdmissionErrorHeader) == httptransport.ModuleForbiddenCode ||
			(json.Unmarshal(data, &denial) == nil && denial.Code == httptransport.ModuleForbiddenCode) {
			return nil, &UnavailableError{Reason: ReasonModuleForbidden}
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

type originTransport struct {
	origin url.URL
	next   http.RoundTripper
}

func (t *originTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil || r.URL.Scheme != t.origin.Scheme || r.URL.Host != t.origin.Host ||
		r.URL.User != nil || (r.Host != "" && r.Host != t.origin.Host) {
		return nil, fmt.Errorf("userdata HTTP: request origin mismatch")
	}
	return t.next.RoundTrip(r)
}

func parseOrigin(raw string, insecure bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Opaque != "" || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("userdata HTTP: a bare HTTP(S) origin is required")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || (!insecure && u.Scheme != "https") {
		return nil, fmt.Errorf("userdata HTTP: HTTPS is required outside insecure dev")
	}
	if insecure && u.Scheme != "http" {
		return nil, fmt.Errorf("userdata HTTP: insecure dev uses HTTP; unset the insecure flag for HTTPS")
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, "\\% \t\r\n") {
		return nil, fmt.Errorf("userdata HTTP: invalid origin host")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return nil, fmt.Errorf("userdata HTTP: unspecified address is not a dial target")
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return nil, fmt.Errorf("userdata HTTP: invalid IPv6 origin")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("userdata HTTP: invalid origin port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("userdata HTTP: empty origin port")
	}
	return u, nil
}

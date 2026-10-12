package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muxcore-Media/userdata-local/conditional"
	"github.com/Muxcore-Media/userdata-local/httpclient"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/httptransport"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	"github.com/Muxcore-Media/userdata-local/internal/testtls"
	"github.com/Muxcore-Media/userdata-local/parental"
)

func conditionalV3HTTPFixture(t *testing.T) (http.Handler, *policyHTTPProvider, *store.Store) {
	t.Helper()
	handler, provider, st := policyHTTPFixture(t)
	provider.identities["kid"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "kid"}}
	return handler, provider, st
}

func conditionalV3HTTPRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer kid")
	req.Header.Set(auth.UserIDHeader, "kid")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func newConditionalV3Body(t *testing.T, d conditional.Document, patch string) (conditional.Update, []byte) {
	t.Helper()
	update := conditional.Update{Contract: conditional.CapabilityV3, RequestID: "http-1", Expected: d.Precondition(), Mutation: json.RawMessage(patch)}
	raw, err := conditional.Encode(update)
	if err != nil {
		t.Fatal(err)
	}
	return update, raw
}

func TestConditionalV3HTTPLifecycleAndAtomicRefusals(t *testing.T) {
	handler, provider, st := conditionalV3HTTPFixture(t)
	read := conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, "")
	doc, err := conditional.DecodeDocumentV3(read.Body.Bytes())
	if err != nil || read.Code != 200 || doc.Exists || doc.UserID != "kid" || doc.TenantID != "" {
		t.Fatalf("GET: %d %s %v", read.Code, read.Body, err)
	}
	if _, revision, err := st.Get("kid"); err != nil || revision != 0 {
		t.Fatal("GET created data")
	}
	update, body := newConditionalV3Body(t, doc, `{"prefs":{"display":{"theme":"light"},"future":9007199254740993},"favorites":{"set":{"x":{"id":"x"}}},"wantToWatch":{"set":{"later":{"id":"later"}}},"playlists":[],"queue":[],"progress":{"report":{"movie":{"positionSec":95,"durationSec":100,"observedAt":"2026-10-12T10:00:00+01:00","metadata":{"future":9007199254740993}}},"set_watched":{"movie":false},"remove":["absent"]}}`)
	w := conditionalV3HTTPRequest(handler, "PUT", conditional.PathV3, string(body))
	ack, err := conditional.DecodeAcknowledgmentV3(w.Body.Bytes(), update)
	if err != nil || w.Code != 200 {
		t.Fatalf("PUT: %d %s %v", w.Code, w.Body, err)
	}
	fresh := conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, "")
	got, err := conditional.DecodeDocumentV3(fresh.Body.Bytes())
	if err != nil || !bytes.Equal(got.Blob, ack.Blob) || got.Revision != "1" {
		t.Fatal("GET not authoritative")
	}
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"stale", "PUT", conditional.PathV3, string(body), 409, "userdata.revision_conflict"},
		{"query", "GET", conditional.PathV3 + "?x=1", "", 400, "userdata.invalid_query"},
		{"bare query", "PUT", conditional.PathV3 + "?", string(body), 400, "userdata.invalid_query"},
		{"method", "POST", conditional.PathV3, "{}", 405, "userdata.method_not_allowed"},
		{"empty body", "PUT", conditional.PathV3, "", 400, "userdata.invalid_body"},
		{"absent expected", "PUT", conditional.PathV3, `{"contract":"userdata.conditional.v3"}`, 428, "userdata.precondition_required"},
		{"protocol", "PUT", conditional.PathV3, `{"contract":"other"}`, 400, "userdata.protocol_unsupported"},
		{"too large", "PUT", conditional.PathV3, strings.Repeat(" ", conditional.MaxRequestBytes+1), 413, "userdata.too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := conditionalV3HTTPRequest(handler, tc.method, tc.path, tc.body)
			if result.Code != tc.status || result.Body.String() != `{"code":"`+tc.code+`"}`+"\n" || result.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %s", result.Code, result.Body)
			}
			if strings.Contains(result.Body.String(), "revision\"") {
				t.Fatal("error exposed revision or ACK")
			}
			if tc.status == 405 && result.Header().Get("Allow") != "GET, PUT" {
				t.Fatal("missing Allow")
			}
		})
	}
	_, mixed := newConditionalV3Body(t, got, `{"prefs":{"display":{"theme":"dark"}},"progress":null}`)
	if refused := conditionalV3HTTPRequest(handler, "PUT", conditional.PathV3, string(mixed)); refused.Code != 400 || !strings.Contains(refused.Body.String(), "userdata.invalid_body") {
		t.Fatal(refused.Body)
	}
	after := conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, "")
	if !bytes.Equal(after.Body.Bytes(), fresh.Body.Bytes()) {
		t.Fatal("refusals changed data/revision")
	}
	// Fresh auth tenant changes refuse the next request, even with an unchanged bearer.
	provider.identities["kid"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "kid", TenantID: "other"}}
	if denied := conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, ""); denied.Code != 403 || !strings.Contains(denied.Body.String(), "userdata.scope_unsupported") {
		t.Fatal(denied.Body)
	}
	provider.identities["kid"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "kid"}}
	provider.unavailable = true
	if unavailable := conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, ""); unavailable.Code != 503 || !strings.Contains(unavailable.Body.String(), "userdata.identity_unavailable") {
		t.Fatal(unavailable.Body)
	}
	provider.unavailable = false
	delete(provider.identities, "kid")
	if denied := conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, ""); denied.Code != 401 {
		t.Fatal(denied.Body)
	}
}

func TestConditionalV3HTTPPreservesErasureAndLegacyRoutes(t *testing.T) {
	handler, _, st := conditionalV3HTTPFixture(t)
	w := conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, "")
	doc, _ := conditional.DecodeDocumentV3(w.Body.Bytes())
	_, body := newConditionalV3Body(t, doc, `{"queue":[]}`)
	st.SetErasureLookup(func(id string) bool { return id == "kid" })
	w = conditionalV3HTTPRequest(handler, "PUT", conditional.PathV3, string(body))
	if w.Code != 410 || !strings.Contains(w.Body.String(), "userdata.account_erased") {
		t.Fatalf("seen: %d %s", w.Code, w.Body)
	}
	st.SetErasureLookup(nil)
	if _, err := st.EraseUser(context.Background(), "http-erasure", "kid", ""); err != nil {
		t.Fatal(err)
	}
	w = conditionalV3HTTPRequest(handler, "PUT", conditional.PathV3, string(body))
	if w.Code != 410 {
		t.Fatal("durable erasure not enforced")
	}
	legacy := conditionalV3HTTPRequest(handler, "PUT", "/api/userdata", `{"queue":[]}`)
	if legacy.Code != 410 {
		t.Fatal("legacy erasure boundary changed")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	w = conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "userdata.storage_unavailable") {
		t.Fatal(w.Body)
	}
}

func TestConditionalV3RealMTLSCheckedClientRoundtrip(t *testing.T) {
	handler, identities, st := conditionalV3HTTPFixture(t)
	identities.identities["admin"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "admin"}, Roles: []string{"admin"}}
	ca := testtls.NewCA(t)
	provider := ca.Issue(t, "userdata-local")
	tlsConfig, err := httptransport.ServerConfig(httptransport.Config{Profile: "household", ModuleID: "userdata-local", CertFile: provider.CertFile, KeyFile: provider.KeyFile, CAFile: ca.File})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(httptransport.Admit(handler))
	s.TLS = tlsConfig
	s.StartTLS()
	defer s.Close()
	makeClient := func(cn string) *httpclient.Client {
		id := ca.Issue(t, cn)
		c, err := httpclient.New(httpclient.Config{Origin: s.URL, Profile: "household", ModuleID: cn, CertFile: id.CertFile, KeyFile: id.KeyFile, CAFile: ca.File})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.CloseIdleConnections)
		return c
	}
	headers := http.Header{"Authorization": {"Bearer kid"}, auth.UserIDHeader: {"kid"}}
	client := makeClient("media-ui")
	resp, err := client.Do(context.Background(), httpclient.GetUserdataV3, headers, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	doc, err := conditional.DecodeDocumentV3(raw)
	if err != nil {
		t.Fatal(err)
	}
	update, body := newConditionalV3Body(t, doc, `{"progress":{"report":{"movie":{"positionSec":9007199254740993,"durationSec":0}},"set_watched":{"movie":false}}}`)
	resp, err = client.Do(context.Background(), httpclient.PutUserdataV3, headers, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	ack, err := conditional.DecodeAcknowledgmentV3(raw, update)
	if err != nil || !bytes.Contains(ack.Blob, []byte("9007199254740993")) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("actual TLS ACK: %s %v", raw, err)
	}
	for _, cn := range []string{"admin-ui", "userdata-local", "health-monitor", "jellyfin", "muxcore"} {
		for _, op := range []httpclient.Operation{httpclient.GetUserdataV3, httpclient.PutUserdataV3} {
			response, err := makeClient(cn).Do(context.Background(), op, headers, bytes.NewReader(body))
			var unavailable *httpclient.UnavailableError
			if response != nil || !errors.As(err, &unavailable) || unavailable.Reason != httpclient.ReasonModuleForbidden {
				t.Fatalf("admitted %s %v: %v", cn, op, err)
			}
		}
	}
	// Use an authenticated media-ui socket for paths unavailable as fixed client
	// operations, while retaining real admission, bearer checks and storage.
	media := ca.Issue(t, "media-ui")
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: ca.Pool, ServerName: "userdata-local", Certificates: []tls.Certificate{media.Pair}}}
	defer transport.CloseIdleConnections()
	rawClient := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	refuse := func(method, path string, requestHeaders http.Header, status int, code string) {
		t.Helper()
		req, err := http.NewRequest(method, s.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = requestHeaders.Clone()
		resp, err := rawClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status || string(raw) != `{"code":"`+code+`"}`+"\n" || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("TLS %s %s: %d %s %v", method, path, resp.StatusCode, raw, err)
		}
	}
	for _, method := range []string{"GET", "PUT"} {
		for _, query := range []string{"?", "?x=1"} {
			refuse(method, conditional.PathV3+query, headers, 400, "userdata.invalid_query")
		}
		for _, tc := range []struct {
			headers http.Header
			status  int
			code    string
		}{
			{http.Header{}, 401, "userdata.unauthenticated"},
			{http.Header{"X-Auth-Token": {"kid"}, auth.UserIDHeader: {"kid"}}, 401, "userdata.unauthenticated"},
			{http.Header{"Authorization": {"Bearer kid", "Bearer kid"}, auth.UserIDHeader: {"kid"}}, 401, "userdata.unauthenticated"},
			{http.Header{"Authorization": {"Bearer kid"}}, 403, "userdata.forbidden"},
			{http.Header{"Authorization": {"Bearer kid"}, auth.UserIDHeader: {"kid", "kid"}}, 403, "userdata.forbidden"},
			{http.Header{"Authorization": {"Bearer admin"}, auth.UserIDHeader: {"kid"}}, 403, "userdata.forbidden"},
		} {
			refuse(method, conditional.PathV3, tc.headers, tc.status, tc.code)
		}
	}
	identities.identities["kid"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "kid", TenantID: "other"}}
	spoofedTenant := headers.Clone()
	spoofedTenant.Set("X-MuxCore-Tenant-Id", "")
	refuse("PUT", conditional.PathV3, spoofedTenant, 403, "userdata.scope_unsupported")
	identities.identities["kid"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "kid"}}
	identities.unavailable = true
	refuse("GET", conditional.PathV3, headers, 503, "userdata.identity_unavailable")
	identities.unavailable = false
	delete(identities.identities, "kid")
	refuse("PUT", conditional.PathV3, headers, 401, "userdata.unauthenticated")
	after, err := st.GetConditionalV3(context.Background(), "kid")
	if err != nil || after.Revision != ack.Revision || !bytes.Equal(after.Blob, ack.Blob) {
		t.Fatal("TLS refusals changed committed data", err)
	}
}

func TestConditionalV3LostAckRemainsUncertainAfterReadAndConflict(t *testing.T) {
	handler, _, st := conditionalV3HTTPFixture(t)
	d, err := st.GetConditionalV3(context.Background(), "kid")
	if err != nil {
		t.Fatal(err)
	}
	update, body := newConditionalV3Body(t, d, `{"progress":{"set_watched":{"movie":true}}}`)
	var drop atomic.Bool
	drop.Store(true)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && drop.Load() {
			record := httptest.NewRecorder()
			handler.ServeHTTP(record, r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	client, err := httpclient.New(httpclient.Config{Origin: s.URL, ModuleID: "media-ui", Profile: "dev", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	headers := http.Header{"Authorization": {"Bearer kid"}, auth.UserIDHeader: {"kid"}}
	if resp, err := client.Do(context.Background(), httpclient.PutUserdataV3, headers, bytes.NewReader(body)); resp != nil || !errors.Is(err, httpclient.ErrUnavailable) {
		t.Fatalf("lost ACK became success: %v", err)
	}
	// Handler dispatch is complete once the closed connection is observed.
	drop.Store(false)
	resp, err := client.Do(context.Background(), httpclient.GetUserdataV3, headers, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	observed, err := conditional.DecodeDocumentV3(raw)
	if err != nil || observed.Revision != "1" {
		t.Fatalf("committed state unavailable: %s %v", raw, err)
	}
	if _, err := conditional.DecodeAcknowledgmentV3(raw, update); err == nil {
		t.Fatal("GET resolved uncertain write")
	}
	resp, err = client.Do(context.Background(), httpclient.PutUserdataV3, headers, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("retry was deduplicated/rebased: %d", resp.StatusCode)
	}
}

func TestConditionalV3FreshSelfBindingAndVersionBoundary(t *testing.T) {
	handler, provider, st := conditionalV3HTTPFixture(t)
	provider.identities["admin"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "admin"}, Roles: []string{"admin"}}
	for _, tc := range []struct {
		name   string
		header http.Header
		status int
	}{
		{"missing", http.Header{}, 401},
		{"legacy token", http.Header{"X-Auth-Token": {"kid"}, auth.UserIDHeader: {"kid"}}, 401},
		{"multiple bearer", http.Header{"Authorization": {"Bearer kid", "Bearer kid"}, auth.UserIDHeader: {"kid"}}, 401},
		{"selector missing", http.Header{"Authorization": {"Bearer kid"}}, 403},
		{"selector multiple", http.Header{"Authorization": {"Bearer kid"}, auth.UserIDHeader: {"kid", "kid"}}, 403},
		{"other user", http.Header{"Authorization": {"Bearer kid"}, auth.UserIDHeader: {"other"}}, 403},
		{"admin other", http.Header{"Authorization": {"Bearer admin"}, auth.UserIDHeader: {"kid"}}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", conditional.PathV3, nil)
			req.Header = tc.header
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	v3 := conditionalV3HTTPRequest(handler, "GET", conditional.PathV3, "")
	doc, err := conditional.DecodeDocumentV3(v3.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	_, body := newConditionalV3Body(t, doc, `{"progress":{"set_watched":{"x":true}}}`)
	v2Refusal := conditionalV3HTTPRequest(handler, "PUT", conditional.Path, string(body))
	if v2Refusal.Code != 400 || !strings.Contains(v2Refusal.Body.String(), "userdata.protocol_unsupported") {
		t.Fatal("v2 accepted v3 request")
	}
	v2Body := bytes.Replace(body, []byte(conditional.CapabilityV3), []byte(conditional.Capability), 1)
	v3Refusal := conditionalV3HTTPRequest(handler, "PUT", conditional.PathV3, string(v2Body))
	if v3Refusal.Code != 400 || !strings.Contains(v3Refusal.Body.String(), "userdata.protocol_unsupported") {
		t.Fatal("v3 accepted v2 request")
	}
	v2Progress := conditionalV3HTTPRequest(handler, "PUT", conditional.Path, string(v2Body))
	if v2Progress.Code != 400 || !strings.Contains(v2Progress.Body.String(), "userdata.mutation_unsupported") {
		t.Fatal("v2 progress contract changed")
	}
	if _, revision, err := st.Get("kid"); err != nil || revision != 0 {
		t.Fatal("refusals created row")
	}
	for _, path := range []string{conditional.PathV3 + "?", conditional.PathV3 + "?x=1"} {
		for _, method := range []string{"GET", "PUT"} {
			w := conditionalV3HTTPRequest(handler, method, path, string(body))
			if w.Code != 400 || w.Body.String() != "{\"code\":\"userdata.invalid_query\"}\n" {
				t.Fatalf("query accepted %s %s: %d %s", method, path, w.Code, w.Body)
			}
		}
	}
}

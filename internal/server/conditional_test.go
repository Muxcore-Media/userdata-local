package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Muxcore-Media/userdata-local/conditional"
	"github.com/Muxcore-Media/userdata-local/httpclient"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/httptransport"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	"github.com/Muxcore-Media/userdata-local/internal/testtls"
	"github.com/Muxcore-Media/userdata-local/parental"
)

func conditionalHTTPFixture(t *testing.T) (http.Handler, *policyHTTPProvider, *store.Store) {
	t.Helper()
	handler, provider, st := policyHTTPFixture(t)
	provider.identities["kid"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "kid"}}
	return handler, provider, st
}

func conditionalHTTPRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer kid")
	req.Header.Set(auth.UserIDHeader, "kid")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func newConditionalBody(t *testing.T, d conditional.Document, patch string) (conditional.Update, []byte) {
	t.Helper()
	update := conditional.Update{Contract: conditional.Capability, RequestID: "http-1", Expected: d.Precondition(), Mutation: json.RawMessage(patch)}
	raw, err := conditional.Encode(update)
	if err != nil {
		t.Fatal(err)
	}
	return update, raw
}

func TestConditionalHTTPLifecycleAndAtomicRefusals(t *testing.T) {
	handler, provider, st := conditionalHTTPFixture(t)
	read := conditionalHTTPRequest(handler, "GET", conditional.Path, "")
	doc, err := conditional.DecodeDocument(read.Body.Bytes())
	if err != nil || read.Code != 200 || doc.Exists || doc.UserID != "kid" || doc.TenantID != "" {
		t.Fatalf("GET: %d %s %v", read.Code, read.Body, err)
	}
	if _, revision, err := st.Get("kid"); err != nil || revision != 0 {
		t.Fatal("GET created data")
	}
	update, body := newConditionalBody(t, doc, `{"prefs":{"display":{"theme":"light"},"future":9007199254740993},"favorites":{"set":{"x":{"id":"x"}}},"wantToWatch":{"set":{"later":{"id":"later"}}},"playlists":[],"queue":[]}`)
	w := conditionalHTTPRequest(handler, "PUT", conditional.Path, string(body))
	ack, err := conditional.DecodeAcknowledgment(w.Body.Bytes(), update)
	if err != nil || w.Code != 200 {
		t.Fatalf("PUT: %d %s %v", w.Code, w.Body, err)
	}
	fresh := conditionalHTTPRequest(handler, "GET", conditional.Path, "")
	got, err := conditional.DecodeDocument(fresh.Body.Bytes())
	if err != nil || !bytes.Equal(got.Blob, ack.Blob) || got.Revision != "1" {
		t.Fatal("GET not authoritative")
	}
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"stale", "PUT", conditional.Path, string(body), 409, "userdata.revision_conflict"},
		{"query", "GET", conditional.Path + "?x=1", "", 400, "userdata.invalid_query"},
		{"bare query", "PUT", conditional.Path + "?", string(body), 400, "userdata.invalid_query"},
		{"method", "POST", conditional.Path, "{}", 405, "userdata.method_not_allowed"},
		{"empty body", "PUT", conditional.Path, "", 400, "userdata.invalid_body"},
		{"absent expected", "PUT", conditional.Path, `{"contract":"userdata.conditional.v2"}`, 428, "userdata.precondition_required"},
		{"protocol", "PUT", conditional.Path, `{"contract":"other"}`, 400, "userdata.protocol_unsupported"},
		{"too large", "PUT", conditional.Path, strings.Repeat(" ", conditional.MaxRequestBytes+1), 413, "userdata.too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := conditionalHTTPRequest(handler, tc.method, tc.path, tc.body)
			if result.Code != tc.status || !strings.Contains(result.Body.String(), tc.code) || result.Header().Get("Cache-Control") != "no-store" {
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
	_, mixed := newConditionalBody(t, got, `{"prefs":{"display":{"theme":"dark"}},"progress":{}}`)
	if refused := conditionalHTTPRequest(handler, "PUT", conditional.Path, string(mixed)); refused.Code != 400 || !strings.Contains(refused.Body.String(), "userdata.mutation_unsupported") {
		t.Fatal(refused.Body)
	}
	after := conditionalHTTPRequest(handler, "GET", conditional.Path, "")
	if !bytes.Equal(after.Body.Bytes(), fresh.Body.Bytes()) {
		t.Fatal("refusals changed data/revision")
	}
	// Fresh auth tenant changes refuse the next request, even with an unchanged bearer.
	provider.identities["kid"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "kid", TenantID: "other"}}
	if denied := conditionalHTTPRequest(handler, "GET", conditional.Path, ""); denied.Code != 403 || !strings.Contains(denied.Body.String(), "userdata.scope_unsupported") {
		t.Fatal(denied.Body)
	}
	provider.identities["kid"] = auth.PolicyIdentity{Scope: parental.Scope{UserID: "kid"}}
	provider.unavailable = true
	if unavailable := conditionalHTTPRequest(handler, "GET", conditional.Path, ""); unavailable.Code != 503 || !strings.Contains(unavailable.Body.String(), "userdata.identity_unavailable") {
		t.Fatal(unavailable.Body)
	}
	provider.unavailable = false
	delete(provider.identities, "kid")
	if denied := conditionalHTTPRequest(handler, "GET", conditional.Path, ""); denied.Code != 401 {
		t.Fatal(denied.Body)
	}
}

func TestConditionalHTTPPreservesErasureAndLegacyRoutes(t *testing.T) {
	handler, _, st := conditionalHTTPFixture(t)
	w := conditionalHTTPRequest(handler, "GET", conditional.Path, "")
	doc, _ := conditional.DecodeDocument(w.Body.Bytes())
	_, body := newConditionalBody(t, doc, `{"queue":[]}`)
	st.SetErasureLookup(func(id string) bool { return id == "kid" })
	w = conditionalHTTPRequest(handler, "PUT", conditional.Path, string(body))
	if w.Code != 410 || !strings.Contains(w.Body.String(), "userdata.account_erased") {
		t.Fatalf("seen: %d %s", w.Code, w.Body)
	}
	st.SetErasureLookup(nil)
	if _, err := st.EraseUser(context.Background(), "http-erasure", "kid", ""); err != nil {
		t.Fatal(err)
	}
	w = conditionalHTTPRequest(handler, "PUT", conditional.Path, string(body))
	if w.Code != 410 {
		t.Fatal("durable erasure not enforced")
	}
	legacy := conditionalHTTPRequest(handler, "PUT", "/api/userdata", `{"queue":[]}`)
	if legacy.Code != 410 {
		t.Fatal("legacy erasure boundary changed")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	w = conditionalHTTPRequest(handler, "GET", conditional.Path, "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "userdata.storage_unavailable") {
		t.Fatal(w.Body)
	}
}

func TestConditionalRealMTLSCheckedClientRoundtrip(t *testing.T) {
	handler, _, _ := conditionalHTTPFixture(t)
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
	resp, err := client.Do(context.Background(), httpclient.GetUserdataV2, headers, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	doc, err := conditional.DecodeDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	update, body := newConditionalBody(t, doc, `{"prefs":{"display":{"theme":"light"},"future":9007199254740993}}`)
	resp, err = client.Do(context.Background(), httpclient.PutUserdataV2, headers, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	ack, err := conditional.DecodeAcknowledgment(raw, update)
	if err != nil || !bytes.Contains(ack.Blob, []byte("9007199254740993")) {
		t.Fatalf("actual TLS ACK: %s %v", raw, err)
	}
	for _, cn := range []string{"admin-ui", "userdata-local", "health-monitor", "jellyfin", "muxcore"} {
		for _, op := range []httpclient.Operation{httpclient.GetUserdataV2, httpclient.PutUserdataV2} {
			if response, err := makeClient(cn).Do(context.Background(), op, headers, bytes.NewReader(body)); response != nil || !errors.Is(err, httpclient.ErrUnavailable) {
				t.Fatalf("admitted %s %v: %v", cn, op, err)
			}
		}
	}
}

func TestConditionalLostAckRemainsUncertainAfterReadAndConflict(t *testing.T) {
	handler, _, st := conditionalHTTPFixture(t)
	d, err := st.GetConditional(context.Background(), "kid")
	if err != nil {
		t.Fatal(err)
	}
	update, body := newConditionalBody(t, d, `{"queue":[]}`)
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
	if resp, err := client.Do(context.Background(), httpclient.PutUserdataV2, headers, bytes.NewReader(body)); resp != nil || !errors.Is(err, httpclient.ErrUnavailable) {
		t.Fatalf("lost ACK became success: %v", err)
	}
	// Handler dispatch is complete once the closed connection is observed.
	drop.Store(false)
	resp, err := client.Do(context.Background(), httpclient.GetUserdataV2, headers, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	observed, err := conditional.DecodeDocument(raw)
	if err != nil || observed.Revision != "1" {
		t.Fatalf("committed state unavailable: %s %v", raw, err)
	}
	if _, err := conditional.DecodeAcknowledgment(raw, update); err == nil {
		t.Fatal("GET resolved uncertain write")
	}
	resp, err = client.Do(context.Background(), httpclient.PutUserdataV2, headers, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("retry was deduplicated/rebased: %d", resp.StatusCode)
	}
}

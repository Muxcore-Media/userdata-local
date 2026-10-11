package internal

import (
	"context"
	"crypto/tls"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	authv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/auth/v1"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure/erasuretest"
	"github.com/Muxcore-Media/userdata-local/internal/auth"
	"github.com/Muxcore-Media/userdata-local/internal/server"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	"github.com/Muxcore-Media/userdata-local/internal/testtls"
	userdatav1 "github.com/Muxcore-Media/userdata-local/proto/gen/muxcore/userdata/v1"
)

// These regressions use real SQLite, the real server handlers and loopback TLS
// on both the userdata and ledger RPCs. Only identity/discovery are fixtures.
// Sweeps are explicit: starting Module.Start's scheduler would make the interval
// between observing a tombstone and retrying a failed apply nondeterministic.
type erasureRPCFixture struct {
	module   *Module
	provider *erasuretest.Provider
	raw      *sql.DB
	session  userdatav1.UserDataServiceClient
	mesh     userdatav1.UserDataServiceClient
}

func newErasureRPCFixture(t *testing.T) *erasureRPCFixture {
	t.Helper()
	clearMeshEnv(t)
	ca := testtls.NewCA(t)
	ownerIdentity := ca.Issue(t, testOwnerID)
	transportEnv(t, ca, ownerIdentity)
	dbPath := filepath.Join(t.TempDir(), "userdata.db")
	seedErasureDB(t, dbPath)

	provider := erasuretest.NewProvider()
	provider.Allowed = map[string]bool{testOwnerID: true}
	provider.Required = []string{testOwnerID}
	providerIdentity := ca.Issue(t, testProviderID)
	providerAddr := erasuretest.ServeTLS(t, provider, providerIdentity.CertFile, providerIdentity.KeyFile, ca.File)
	dialer := &erasure.ProviderDialer{
		Discovery: erasuretest.NewDiscovery(erasuretest.Module(testProviderID, providerAddr)),
		CertFile:  ownerIdentity.CertFile,
		KeyFile:   ownerIdentity.KeyFile,
		CAFile:    ca.File,
	}
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: dbPath,
		AuthProvider: staticTokens(), ErasureDialer: dialer, ErasureTune: fastTune})
	if err := m.Init(context.Background()); err != nil {
		t.Fatalf("fixture setup: Init: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})
	if m.grpcTLS == nil || m.Reconciler() == nil {
		t.Fatal("fixture setup: missing household TLS or reconciler")
	}
	// Serve the actual registered handlers with the module's transport config,
	// without Module.Start's background sweep. No synthetic TLS peer context.
	m.grpcSrv = grpc.NewServer(grpc.Creds(credentials.NewTLS(m.grpcTLS)))
	m.srv.RegisterWithGRPC(m.grpcSrv)
	grpcServer, grpcListener := m.grpcSrv, m.grpcLis
	go func() { _ = grpcServer.Serve(grpcListener) }()
	client := func(pair *tls.Certificate) userdatav1.UserDataServiceClient {
		t.Helper()
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: ca.Pool, ServerName: testOwnerID}
		if pair != nil {
			cfg.Certificates = []tls.Certificate{*pair}
		}
		conn, err := grpc.NewClient(m.grpcLis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatalf("fixture setup: userdata client: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return userdatav1.NewUserDataServiceClient(conn)
	}
	meshIdentity := ca.Issue(t, "media-ui")
	f := &erasureRPCFixture{module: m, provider: provider, raw: openRaw(t, dbPath),
		session: client(nil), mesh: client(&meshIdentity.Pair)}

	// TLS alone and a spoofed caller header must not accidentally turn the
	// bearer client into a service account. Successful requests below prove
	// these failures are authorization failures, not broken connectivity.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs(auth.CallerIDMetadataKey, "media-ui"))
	if _, err := f.session.Put(ctx, &userdatav1.PutRequest{UserId: "victim", Json: []byte(`{}`)}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("fixture setup: spoofed mesh metadata: %v", err)
	}
	return f
}

func erasureRPCPut(t *testing.T, client userdatav1.UserDataServiceClient, userID, token, body string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if token != "" {
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs(auth.AuthTokenMetadataKey, token))
	}
	_, err := client.Put(ctx, &userdatav1.PutRequest{UserId: userID, Json: []byte(body)})
	return err
}

func (f *erasureRPCFixture) sweep(t *testing.T) (erasure.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return f.module.Reconciler().SweepOnce(ctx)
}

func (f *erasureRPCFixture) blobSnapshot(t *testing.T, userID string) (string, int64) {
	t.Helper()
	var body string
	var revision int64
	if err := f.raw.QueryRow(`SELECT json_blob, revision FROM user_blobs WHERE user_id = ?`, userID).Scan(&body, &revision); err != nil {
		t.Fatalf("fixture observation: blob %q: %v", userID, err)
	}
	return body, revision
}

// Independently enumerate raw keys: the production exact-ID postcondition is
// precisely what the padded-key regression must not use as its only oracle.
func (f *erasureRPCFixture) matchingBlobKeys(t *testing.T, userID string) []string {
	t.Helper()
	rows, err := f.raw.Query(`SELECT user_id FROM user_blobs ORDER BY user_id`)
	if err != nil {
		t.Fatalf("fixture observation: blob keys: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("fixture observation: blob key: %v", err)
		}
		if strings.TrimSpace(key) == userID {
			keys = append(keys, key)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("fixture observation: iterate blob keys: %v", err)
	}
	return keys
}

func (f *erasureRPCFixture) requireBystanders(t *testing.T) {
	t.Helper()
	for _, userID := range []string{"bystander", "other-tenant-user"} {
		blob, revision, err := f.module.store.Get(userID)
		if err != nil || revision < 1 || blob.Favorites["f"].ID != "f" {
			t.Errorf("bystander %q lost: revision=%d blob=%+v err=%v", userID, revision, blob, err)
		}
		var n int
		if err := f.raw.QueryRow(`SELECT COUNT(*) FROM parental_policies WHERE user_id = ?`, userID).Scan(&n); err != nil || n != 1 {
			t.Errorf("bystander policy %q: rows=%d err=%v", userID, n, err)
		}
	}
}

func (f *erasureRPCFixture) requireCleanAcknowledgement(t *testing.T, erasureID string) {
	t.Helper()
	keys := f.matchingBlobKeys(t, "victim")
	references, err := f.module.store.CountUserRows(context.Background(), "victim")
	if err != nil {
		t.Fatalf("fixture observation: victim references: %v", err)
	}
	ack, ok := f.provider.Latest(erasureID, testOwnerID)
	if len(keys) != 0 || references != 0 {
		t.Errorf("erasure retained victim data: blob keys=%q canonical references=%d", keys, references)
		if ok && ack.Outcome == authv1.ErasureOutcome_ERASURE_OUTCOME_OK {
			t.Errorf("false OK acknowledgement while victim data remains: blob keys=%q canonical references=%d", keys, references)
		}
	} else if !ok || ack.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_OK {
		t.Errorf("clean erasure not acknowledged OK: %+v present=%v", ack, ok)
	}
	f.requireBystanders(t)
}

func TestErasureGRPCPaddedIDCannotSurviveOrResurrect(t *testing.T) {
	f := newErasureRPCFixture(t)
	const body = `{"favorites":{"private":{"id":"private"}}}`
	for _, key := range []string{" victim ", "\tvictim\n", "\u2003victim\u00a0"} {
		err := erasureRPCPut(t, f.session, key, "victim-token", body)
		switch status.Code(err) {
		case codes.OK:
			t.Logf("baseline authenticated padded key accepted: %q", key)
		case codes.InvalidArgument:
			// A later canonical-ID fix may reject new padded writes. Preserve
			// the historical-row test without requiring that acceptance: these
			// rows were writable through the pinned release's authenticated API.
			if _, err := f.raw.Exec(`INSERT INTO user_blobs (user_id, json_blob, revision, updated_at) VALUES (?, ?, 1, datetime('now'))`, key, body); err != nil {
				t.Fatalf("fixture setup: legacy padded row %q: %v", key, err)
			}
		default:
			t.Fatalf("fixture setup: authenticated padded write %q: %v", key, err)
		}
	}
	if keys := f.matchingBlobKeys(t, "victim"); len(keys) != 4 {
		t.Fatalf("fixture setup: expected canonical and three historical blob keys, got %q", keys)
	}
	eid := f.provider.AddErasure("victim", "home")
	res, err := f.sweep(t)
	if res.Seen != 1 {
		t.Fatalf("fixture setup: tombstone not observed: %+v %v", res, err)
	}
	if err != nil || res.Failed != 0 {
		t.Errorf("erasure did not complete: %+v %v", res, err)
	}
	f.requireCleanAcknowledgement(t, eid)
	// Replay must not make an already-acknowledged residual row invisible.
	res, err = f.sweep(t)
	if err != nil {
		t.Errorf("replayed erasure: %+v %v", res, err)
	}
	f.requireCleanAcknowledgement(t, eid)

	for _, key := range []string{"victim", " victim ", "\u2003victim\u00a0"} {
		err := erasureRPCPut(t, f.mesh, key, "", body)
		refused := status.Code(err) == codes.FailedPrecondition && status.Convert(err).Message() == server.CodeUserdataAccountErased
		// Either rejection of a noncanonical ID or erased-account refusal is
		// safe; choosing the final canonical-ID contract is separate work.
		if key != "victim" && status.Code(err) == codes.InvalidArgument {
			refused = true
		}
		if !refused {
			t.Errorf("verified-mesh late Put for %q was not refused: %v", key, err)
		}
	}
	if err := erasureRPCPut(t, f.session, "bystander", "bystander-token", `{}`); err != nil {
		t.Errorf("bystander bearer write refused: %v", err)
	}
	f.requireCleanAcknowledgement(t, eid)
}

func TestErasureGRPCLastSeenRefusesWriteAfterApplyRollback(t *testing.T) {
	f := newErasureRPCFixture(t)
	beforeBody, beforeRevision := f.blobSnapshot(t, "victim")
	if _, err := f.raw.Exec(`CREATE TRIGGER inject_failure BEFORE INSERT ON erasure_applied BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatalf("fixture setup: aborting applied-record trigger: %v", err)
	}
	eid := f.provider.AddErasure("victim", "home")
	res, err := f.sweep(t)
	if err == nil || res.Failed != 1 || res.Applied != 0 || !f.module.Reconciler().Erased("victim") {
		t.Fatalf("fixture setup: expected observed tombstone and failed apply: %+v %v seen=%v", res, err, f.module.Reconciler().Erased("victim"))
	}
	ack, ok := f.provider.Latest(eid, testOwnerID)
	if !ok || ack.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_FAILED || ack.Detail != erasure.DetailApplyFailed {
		t.Fatalf("fixture setup: expected FAILED/apply_failed acknowledgement: %+v present=%v", ack, ok)
	}
	for _, accepted := range f.provider.Acks() {
		if accepted.Outcome == authv1.ErasureOutcome_ERASURE_OUTCOME_OK {
			t.Errorf("false OK acknowledgement after failed transaction: %+v", accepted)
		}
	}
	var applied, policies int
	var actor string
	if err := f.raw.QueryRow(`SELECT COUNT(*) FROM erasure_applied`).Scan(&applied); err != nil || applied != 0 {
		t.Fatalf("fixture setup: applied record after rollback: rows=%d err=%v", applied, err)
	}
	if err := f.raw.QueryRow(`SELECT COUNT(*) FROM parental_policies WHERE user_id = 'victim'`).Scan(&policies); err != nil || policies != 1 {
		t.Fatalf("fixture setup: policy not retained by rollback: rows=%d err=%v", policies, err)
	}
	if err := f.raw.QueryRow(`SELECT updated_by FROM parental_policies WHERE user_id = 'bystander'`).Scan(&actor); err != nil || actor != "victim" {
		t.Fatalf("fixture setup: actor not retained by rollback: actor=%q err=%v", actor, err)
	}
	if body, revision := f.blobSnapshot(t, "victim"); body != beforeBody || revision != beforeRevision {
		t.Fatalf("fixture setup: blob not retained by rollback: body=%s revision=%d", body, revision)
	}

	// No bearer is sent. The server admits this caller by its real, CA-verified
	// client certificate; ADR-0035 §3 requires the last-seen ledger to refuse it.
	err = erasureRPCPut(t, f.mesh, "victim", "", `{"favorites":{"late":{"id":"late"}}}`)
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != server.CodeUserdataAccountErased {
		t.Errorf("last-seen victim accepted verified-mesh Put after rollback: %v", err)
	}
	if body, revision := f.blobSnapshot(t, "victim"); body != beforeBody || revision != beforeRevision {
		t.Errorf("late write changed retained pending-erasure blob: revision %d -> %d, body=%s", beforeRevision, revision, body)
	}
	if err := erasureRPCPut(t, f.mesh, "bystander", "", `{}`); err != nil {
		t.Errorf("bystander mesh write refused during pending erasure: %v", err)
	}
	f.requireBystanders(t)
	ack, _ = f.provider.Latest(eid, testOwnerID)
	if ack.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_FAILED {
		t.Errorf("pending erasure no longer FAILED before retry: %+v", ack)
	}

	if _, err := f.raw.Exec(`DROP TRIGGER inject_failure`); err != nil {
		t.Fatalf("fixture teardown: remove aborting trigger: %v", err)
	}
	res, err = f.sweep(t)
	if err != nil || res.Applied != 1 || res.Failed != 0 {
		t.Errorf("retry did not complete: %+v %v", res, err)
	}
	f.requireCleanAcknowledgement(t, eid)
	if n, err := f.module.store.CountUserRows(context.Background(), "victim"); err != nil || n != 0 {
		t.Errorf("retry left victim references: rows=%d err=%v", n, err)
	}
	if err := f.raw.QueryRow(`SELECT updated_by FROM parental_policies WHERE user_id = 'bystander'`).Scan(&actor); err != nil || actor != store.AnonymisedActor {
		t.Errorf("retry did not anonymise policy actor: actor=%q err=%v", actor, err)
	}
	res, err = f.sweep(t)
	if err != nil || res.Applied != 0 || res.Skipped != 1 {
		t.Errorf("completed erasure replay: %+v %v", res, err)
	}
	err = erasureRPCPut(t, f.mesh, "victim", "", `{}`)
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != server.CodeUserdataAccountErased {
		t.Errorf("applied victim accepted verified-mesh Put: %v", err)
	}
	f.requireCleanAcknowledgement(t, eid)
}

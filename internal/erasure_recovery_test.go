package internal

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	authv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/auth/v1"
	"github.com/Muxcore-Media/core/sdk/go/module/erasure"
	"github.com/Muxcore-Media/userdata-local/internal/store"
	"github.com/Muxcore-Media/userdata-local/parental"
)

func TestErasureGRPCRejectsNewPaddedIDsWithoutPersistence(t *testing.T) {
	f := newErasureRPCFixture(t)
	body, revision := f.blobSnapshot(t, "victim")
	for _, userID := range []string{" victim ", "\tvictim\n", "\u2003victim\u00a0", "\u2028victim\u3000"} {
		if err := erasureRPCPut(t, f.session, userID, "victim-token", `{}`); status.Code(err) != codes.InvalidArgument {
			t.Errorf("bearer padded Put(%q): %v", userID, err)
		}
		if err := erasureRPCPut(t, f.mesh, userID, "", `{}`); status.Code(err) != codes.InvalidArgument {
			t.Errorf("mesh padded Put(%q): %v", userID, err)
		}
	}
	if keys := f.matchingBlobKeys(t, "victim"); !reflect.DeepEqual(keys, []string{"victim"}) {
		t.Errorf("rejected puts persisted aliases: %q", keys)
	}
	if got, rev := f.blobSnapshot(t, "victim"); got != body || rev != revision {
		t.Errorf("rejected puts changed canonical data: body=%s revision=%d", got, rev)
	}
	for _, userID := range []string{"victim", "Victim", "victim-next", "victim~p~child", "victim smith", "victim\u200b"} {
		if err := erasureRPCPut(t, f.mesh, userID, "", `{}`); err != nil {
			t.Errorf("canonical mesh Put(%q): %v", userID, err)
		}
	}
	f.requireBystanders(t)
}

func erasureRecordImage(t *testing.T, db *sql.DB, erasureID string) [5]string {
	t.Helper()
	var image [5]string
	if err := db.QueryRow(`SELECT erasure_id, user_id, tenant_id, applied_at, counts_json FROM erasure_applied WHERE erasure_id = ?`, erasureID).
		Scan(&image[0], &image[1], &image[2], &image[3], &image[4]); err != nil {
		t.Fatal(err)
	}
	return image
}

func TestErasureAppliedOKStartupRepairsLegacyBlobAliases(t *testing.T) {
	f := newErasureRPCFixture(t)
	eid := f.provider.AddErasure("victim", "home")
	if res, err := f.sweep(t); err != nil || res.Applied != 1 || res.Acked != 1 {
		t.Fatalf("initial apply: %+v %v", res, err)
	}
	before := erasureRecordImage(t, f.raw, eid)
	ack, ok := f.provider.Latest(eid, testOwnerID)
	if !ok || ack.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_OK || ack.Counts[store.CountUserBlobs] != 1 {
		t.Fatalf("initial OK count: %+v %v", ack, ok)
	}
	// A pre-fix store could retain these rows while recording exactly that OK.
	// Seed the old release's state directly; current APIs must refuse padding.
	for _, key := range []string{" victim ", "\tvictim\n", "\u2003victim\u00a0"} {
		if _, err := f.raw.Exec(`INSERT INTO user_blobs (user_id, json_blob) VALUES (?, ?)`, key, `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.module.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DBPath: f.module.dbPath,
		AuthProvider: staticTokens(), ErasureDialer: f.module.erasureDialer, ErasureTune: fastTune})
	if err := m.Init(context.Background()); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	// No listener is serving and no sweep has run in the new process. Store.New
	// must already have repaired aliases; the provider considers this owner OK.
	if keys := f.matchingBlobKeys(t, "victim"); len(keys) != 0 {
		t.Fatalf("startup left already-acknowledged legacy keys: %q", keys)
	}
	if got := erasureRecordImage(t, f.raw, eid); got != before {
		t.Fatalf("startup changed historical record: got=%q before=%q", got, before)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := m.Reconciler().SweepOnce(ctx)
	if err != nil || res.Applied != 0 || res.Skipped != 1 || res.Acked != 0 {
		t.Fatalf("already-OK replay: %+v %v", res, err)
	}
	if got := erasureRecordImage(t, f.raw, eid); got != before {
		t.Errorf("replay changed historical record: got=%q before=%q", got, before)
	}
	if got, _ := f.provider.Latest(eid, testOwnerID); !reflect.DeepEqual(got, ack) {
		t.Errorf("startup/replay rewrote original acknowledgement: got=%+v before=%+v", got, ack)
	}
	f.module = m
	f.requireCleanAcknowledgement(t, eid)
}

func policyRecordImage(t *testing.T, db *sql.DB, userID string) [4]string {
	t.Helper()
	var image [4]string
	if err := db.QueryRow(`SELECT policy_json, revision, updated_by, updated_at FROM parental_policies WHERE tenant_id = 'home' AND user_id = ?`, userID).
		Scan(&image[0], &image[1], &image[2], &image[3]); err != nil {
		t.Fatal(err)
	}
	return image
}

func TestErasureLastSeenRefusesPolicyTargetAndActorAfterRollback(t *testing.T) {
	f := newErasureRPCFixture(t)
	beforeTarget := policyRecordImage(t, f.raw, "victim")
	beforeActor := policyRecordImage(t, f.raw, "bystander")
	if _, err := f.raw.Exec(`CREATE TRIGGER inject_failure BEFORE INSERT ON erasure_applied BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	eid := f.provider.AddErasure("victim", "home")
	res, err := f.sweep(t)
	if err == nil || res.Failed != 1 || res.Applied != 0 || !f.module.Reconciler().Erased("victim") {
		t.Fatalf("expected seen but unapplied erasure: %+v %v", res, err)
	}
	ack, ok := f.provider.Latest(eid, testOwnerID)
	if !ok || ack.Outcome != authv1.ErasureOutcome_ERASURE_OUTCOME_FAILED || ack.Detail != erasure.DetailApplyFailed {
		t.Fatalf("expected FAILED/apply_failed: %+v %v", ack, ok)
	}
	ctx := context.Background()
	update := parental.Update{ExpectedRevision: 1, Policy: parental.Policy{Version: 1, Mode: "unrestricted"}}
	for _, tc := range []struct{ target, actor string }{{"victim", "parent"}, {"bystander", "victim"}} {
		// This is the shared store write boundary reached after HTTP identity
		// checks. It also covers a request authorized before the deletion.
		if _, err := f.module.store.PutParentalPolicy(ctx, parental.Scope{UserID: tc.target, TenantID: "home"}, tc.actor, update); !errors.Is(err, store.ErrUserErased) {
			t.Errorf("seen target=%q actor=%q write: %v", tc.target, tc.actor, err)
		}
	}
	if got := policyRecordImage(t, f.raw, "victim"); got != beforeTarget {
		t.Errorf("seen target changed retained policy: got=%q before=%q", got, beforeTarget)
	}
	if got := policyRecordImage(t, f.raw, "bystander"); got != beforeActor {
		t.Errorf("seen actor changed retained policy: got=%q before=%q", got, beforeActor)
	}
	if doc, err := f.module.store.PutParentalPolicy(ctx, parental.Scope{UserID: "bystander2", TenantID: "home"}, "parent", update); err != nil || doc.Revision != 2 {
		t.Errorf("normal policy write refused: %+v %v", doc, err)
	}
	if _, err := f.raw.Exec(`DROP TRIGGER inject_failure`); err != nil {
		t.Fatal(err)
	}
	if res, err := f.sweep(t); err != nil || res.Applied != 1 || res.Acked != 1 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	// Durable refusal must still work without any process-local lookup.
	f.module.store.SetErasureLookup(nil)
	for _, tc := range []struct{ target, actor string }{{"victim", "parent"}, {"bystander", "victim"}} {
		if _, err := f.module.store.PutParentalPolicy(ctx, parental.Scope{UserID: tc.target, TenantID: "home"}, tc.actor, update); !errors.Is(err, store.ErrUserErased) {
			t.Errorf("durable target=%q actor=%q write: %v", tc.target, tc.actor, err)
		}
	}
	f.requireCleanAcknowledgement(t, eid)
}

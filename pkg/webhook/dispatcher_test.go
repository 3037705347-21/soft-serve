package webhook

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
)

// testPayload is a minimal EventPayload used to exercise the queue.
type testPayload struct {
	Common
	Ref string `json:"ref" url:"ref"`
}

func newTestContext(t *testing.T) (context.Context, *db.DB, store.Store) {
	t.Helper()
	is := is.New(t)

	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx = config.WithContext(ctx, cfg)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	// Use the same busy-timeout pragma as production (see config.go) so the
	// background dispatcher and the test goroutine don't hit SQLITE_BUSY.
	dbx, err := db.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)")
	is.NoErr(err)
	t.Cleanup(func() { dbx.Close() }) //nolint: errcheck

	is.NoErr(migrate.Migrate(ctx, dbx))

	ds := database.New(ctx, dbx)
	ctx = db.WithContext(ctx, dbx)
	ctx = store.WithContext(ctx, ds)

	return ctx, dbx, ds
}

// usePlainHTTPClient swaps the SSRF-protected client for one that can reach
// the loopback httptest servers used in tests.
func usePlainHTTPClient(t *testing.T) {
	t.Helper()
	old := secureHTTPClient
	secureHTTPClient = &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() { secureHTTPClient = old })
}

func createTestHook(t *testing.T, ctx context.Context, dbx *db.DB, ds store.Store, url string, ct ContentType, active bool, events ...Event) int64 {
	t.Helper()
	is := is.New(t)

	var id int64
	evs := make([]int, len(events))
	for i, e := range events {
		evs[i] = int(e)
	}
	is.NoErr(dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		id, err = ds.CreateWebhook(ctx, tx, 42, url, "s3cret", int(ct), active)
		if err != nil {
			return err
		}
		return ds.CreateWebhookEvents(ctx, tx, id, evs)
	}))
	return id
}

func newTestPayload() testPayload {
	return testPayload{
		Common: Common{
			EventType:  EventPush,
			Repository: Repository{ID: 42, Name: "repo"},
			Sender:     User{ID: 1, Username: "alice"},
		},
		Ref: "refs/heads/main",
	}
}

func pendingCount(t *testing.T, ctx context.Context, ds store.Store, dbx *db.DB, hookID int64) int64 {
	t.Helper()
	is := is.New(t)
	n, err := ds.CountWebhookPendingDeliveriesByWebhookID(ctx, dbx, hookID)
	is.NoErr(err)
	return n
}

func deliveryCount(t *testing.T, ctx context.Context, ds store.Store, dbx *db.DB, hookID int64) int {
	t.Helper()
	is := is.New(t)
	ds2, err := ds.GetWebhookDeliveriesByWebhookID(ctx, dbx, hookID)
	is.NoErr(err)
	return len(ds2)
}

// TestSendEventEnqueuesOnce verifies durable enqueue: one outstanding row
// per (hook, event), encoded per hook content type, and nothing for inactive
// hooks.
func TestSendEventEnqueuesOnce(t *testing.T) {
	is := is.New(t)
	ctx, dbx, ds := newTestContext(t)

	jsonHook := createTestHook(t, ctx, dbx, ds, "https://example.com/json", ContentTypeJSON, true, EventPush)
	formHook := createTestHook(t, ctx, dbx, ds, "https://example.com/form", ContentTypeForm, true, EventPush)
	inactiveHook := createTestHook(t, ctx, dbx, ds, "https://example.com/off", ContentTypeJSON, false, EventPush)

	payload := newTestPayload()
	is.NoErr(SendEvent(ctx, payload))
	// The same event again must not create a second outstanding record.
	is.NoErr(SendEvent(ctx, payload))

	is.Equal(pendingCount(t, ctx, ds, dbx, jsonHook), int64(1))
	is.Equal(pendingCount(t, ctx, ds, dbx, formHook), int64(1))
	is.Equal(pendingCount(t, ctx, ds, dbx, inactiveHook), int64(0))

	// Nothing has been sent yet.
	is.Equal(deliveryCount(t, ctx, ds, dbx, jsonHook), 0)
	is.Equal(deliveryCount(t, ctx, ds, dbx, formHook), 0)

	var row models.WebhookPendingDelivery
	is.NoErr(dbx.GetContext(ctx, &row, dbx.Rebind(
		`SELECT * FROM webhook_pending_deliveries WHERE webhook_id = ?;`), jsonHook))
	is.Equal(row.Event, int(EventPush))
	is.Equal(row.Status, models.WebhookPendingStatusPending)
	is.Equal(row.Attempts, 0)
	if row.RequestBody[0] != '{' {
		t.Fatalf("expected JSON body, got %q", row.RequestBody)
	}

	var formRow models.WebhookPendingDelivery
	is.NoErr(dbx.GetContext(ctx, &formRow, dbx.Rebind(
		`SELECT * FROM webhook_pending_deliveries WHERE webhook_id = ?;`), formHook))
	if formRow.RequestBody == "" || formRow.RequestBody[0] == '{' {
		t.Fatalf("expected url-encoded form body, got %q", formRow.RequestBody)
	}
}

// TestDispatcherDeliversOnFirstTry covers the happy path: the queued event
// is sent once, the attempt is recorded, and the queue row is removed.
func TestDispatcherDeliversOnFirstTry(t *testing.T) {
	is := is.New(t)
	usePlainHTTPClient(t)
	ctx, dbx, ds := newTestContext(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if got := r.Header.Get("X-SoftServe-Event"); got != "push" {
			t.Errorf("unexpected event header: %q", got)
		}
		if r.Header.Get("X-SoftServe-Delivery") == "" {
			t.Error("missing delivery header")
		}
		if r.Header.Get("X-SoftServe-Signature") == "" {
			t.Error("missing signature header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hookID := createTestHook(t, ctx, dbx, ds, srv.URL, ContentTypeJSON, true, EventPush)
	is.NoErr(SendEvent(ctx, newTestPayload()))

	d := NewDispatcher(ctx)
	is.NoErr(d.ProcessOnce(ctx))

	is.Equal(atomic.LoadInt32(&hits), int32(1))
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(0))
	is.Equal(deliveryCount(t, ctx, ds, dbx, hookID), 1)
}

// TestDispatcherRetriesFailedDelivery verifies that a failing receiver is
// retried automatically with every attempt recorded, then completes once it
// recovers.
func TestDispatcherRetriesFailedDelivery(t *testing.T) {
	is := is.New(t)
	usePlainHTTPClient(t)
	ctx, dbx, ds := newTestContext(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hookID := createTestHook(t, ctx, dbx, ds, srv.URL, ContentTypeJSON, true, EventPush)
	is.NoErr(SendEvent(ctx, newTestPayload()))

	clock := time.Unix(2_000_000_000, 0)
	d := NewDispatcher(ctx)
	d.now = func() time.Time { return clock }

	is.NoErr(d.ProcessOnce(ctx))
	is.Equal(atomic.LoadInt32(&hits), int32(1))
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(1))
	is.Equal(deliveryCount(t, ctx, ds, dbx, hookID), 1)

	// The failure is scheduled with backoff; it is not retried early.
	clock = clock.Add(5 * time.Second)
	is.NoErr(d.ProcessOnce(ctx))
	is.Equal(atomic.LoadInt32(&hits), int32(1))

	// After the backoff elapses the retry succeeds.
	clock = clock.Add(time.Hour)
	is.NoErr(d.ProcessOnce(ctx))
	is.Equal(atomic.LoadInt32(&hits), int32(2))
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(0))

	deliveries, err := ds.GetWebhookDeliveriesByWebhookID(ctx, dbx, hookID)
	is.NoErr(err)
	is.Equal(len(deliveries), 2)
	is.Equal(deliveries[0].ResponseStatus, http.StatusServiceUnavailable)
	is.Equal(deliveries[1].ResponseStatus, http.StatusOK)
}

// TestDispatcherBackoffAndDead verifies connection failures back off and the
// row turns dead after the maximum number of attempts.
func TestDispatcherBackoffAndDead(t *testing.T) {
	is := is.New(t)
	usePlainHTTPClient(t)
	ctx, dbx, ds := newTestContext(t)

	// Nothing listens here; with the plain test client this is a refused
	// connection rather than an SSRF block.
	hookID := createTestHook(t, ctx, dbx, ds, "http://127.0.0.1:1/hook", ContentTypeJSON, true, EventPush)
	is.NoErr(SendEvent(ctx, newTestPayload()))

	clock := time.Unix(2_000_000_000, 0)
	d := NewDispatcher(ctx)
	d.now = func() time.Time { return clock }

	is.NoErr(d.ProcessOnce(ctx))

	var row models.WebhookPendingDelivery
	is.NoErr(dbx.GetContext(ctx, &row, dbx.Rebind(
		`SELECT * FROM webhook_pending_deliveries WHERE webhook_id = ?;`), hookID))
	is.Equal(row.Status, models.WebhookPendingStatusPending)
	is.Equal(row.Attempts, 1)
	is.Equal(row.NextRetryAt, clock.Unix()+int64(retryBackoff(1).Seconds()))
	is.Equal(row.ClaimedAt, sql.NullInt64{})

	// Drive the remaining attempts, always jumping past the backoff.
	for row.Attempts < maxAttempts {
		clock = clock.Add(time.Hour)
		is.NoErr(d.ProcessOnce(ctx))
		is.NoErr(dbx.GetContext(ctx, &row, dbx.Rebind(
			`SELECT * FROM webhook_pending_deliveries WHERE webhook_id = ?;`), hookID))
	}

	is.Equal(row.Status, models.WebhookPendingStatusDead)
	is.Equal(row.Attempts, maxAttempts)
	is.Equal(deliveryCount(t, ctx, ds, dbx, hookID), maxAttempts)

	deliveries, err := ds.GetWebhookDeliveriesByWebhookID(ctx, dbx, hookID)
	is.NoErr(err)
	for _, dl := range deliveries {
		if !dl.RequestError.Valid || dl.RequestError.String == "" {
			t.Error("expected each failed attempt to record its request error")
		}
	}

	// Dead rows are not outstanding and do not block a fresh identical event.
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(0))
}

// TestDispatcherReclaimsStaleInFlight verifies a delivery interrupted by a
// crash/restart is picked up and completed.
func TestDispatcherReclaimsStaleInFlight(t *testing.T) {
	is := is.New(t)
	usePlainHTTPClient(t)
	ctx, dbx, ds := newTestContext(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hookID := createTestHook(t, ctx, dbx, ds, srv.URL, ContentTypeJSON, true, EventPush)
	is.NoErr(SendEvent(ctx, newTestPayload()))

	// Simulate a dispatcher that claimed the row and died mid-send.
	_, err := dbx.ExecContext(ctx, dbx.Rebind(
		`UPDATE webhook_pending_deliveries SET status = ?, claimed_at = ? WHERE webhook_id = ?;`),
		models.WebhookPendingStatusInFlight, time.Now().Add(-time.Hour).Unix(), hookID)
	is.NoErr(err)

	d := NewDispatcher(ctx)
	is.NoErr(d.ProcessOnce(ctx))

	is.Equal(atomic.LoadInt32(&hits), int32(1))
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(0))
	is.Equal(deliveryCount(t, ctx, ds, dbx, hookID), 1)
}

// TestDispatcherHoldsInactiveWebhook verifies a deactivated webhook generates
// no traffic, keeps the event and resumes delivery when reactivated.
func TestDispatcherHoldsInactiveWebhook(t *testing.T) {
	is := is.New(t)
	usePlainHTTPClient(t)
	ctx, dbx, ds := newTestContext(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hookID := createTestHook(t, ctx, dbx, ds, srv.URL, ContentTypeJSON, true, EventPush)
	is.NoErr(SendEvent(ctx, newTestPayload()))

	is.NoErr(ds.UpdateWebhookByID(ctx, dbx, 42, hookID, srv.URL, "s3cret", int(ContentTypeJSON), false))

	clock := time.Unix(2_000_000_000, 0)
	d := NewDispatcher(ctx)
	d.now = func() time.Time { return clock }
	is.NoErr(d.ProcessOnce(ctx))

	is.Equal(atomic.LoadInt32(&hits), int32(0))
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(1))
	is.Equal(deliveryCount(t, ctx, ds, dbx, hookID), 0)

	// Still within the inactive hold delay: no request.
	clock = clock.Add(inactiveRetryDelay - time.Second)
	is.NoErr(d.ProcessOnce(ctx))
	is.Equal(atomic.LoadInt32(&hits), int32(0))

	// Reactivate and pass the hold delay: delivery goes out.
	is.NoErr(ds.UpdateWebhookByID(ctx, dbx, 42, hookID, srv.URL, "s3cret", int(ContentTypeJSON), true))
	clock = clock.Add(2 * time.Second)
	is.NoErr(d.ProcessOnce(ctx))
	is.Equal(atomic.LoadInt32(&hits), int32(1))
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(0))
}

// TestDispatcherDropsOrphanedDelivery verifies a queued event for a deleted
// webhook is removed rather than retried forever.
func TestDispatcherDropsOrphanedDelivery(t *testing.T) {
	is := is.New(t)
	usePlainHTTPClient(t)
	ctx, dbx, ds := newTestContext(t)

	hookID := createTestHook(t, ctx, dbx, ds, "https://example.com/hook", ContentTypeJSON, true, EventPush)
	is.NoErr(SendEvent(ctx, newTestPayload()))
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(1))

	is.NoErr(ds.DeleteWebhookByID(ctx, dbx, hookID))

	d := NewDispatcher(ctx)
	is.NoErr(d.ProcessOnce(ctx))

	var n int
	is.NoErr(dbx.GetContext(ctx, &n, dbx.Rebind(
		`SELECT COUNT(*) FROM webhook_pending_deliveries WHERE webhook_id = ?;`), hookID))
	is.Equal(n, 0)
}

// TestStartDispatcherWakeAndStop exercises the background loop: an enqueue
// notification triggers delivery without waiting for the poll interval, and
// Stop cleanly unregisters and terminates the dispatcher.
func TestStartDispatcherWakeAndStop(t *testing.T) {
	is := is.New(t)
	usePlainHTTPClient(t)
	ctx, dbx, ds := newTestContext(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hookID := createTestHook(t, ctx, dbx, ds, srv.URL, ContentTypeJSON, true, EventPush)

	d := StartDispatcher(ctx)
	is.NoErr(SendEvent(ctx, newTestPayload()))

	deadline := time.Now().Add(3 * time.Second)
	for pendingCount(t, ctx, ds, dbx, hookID) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(0))
	is.Equal(atomic.LoadInt32(&hits), int32(1))

	d.Stop()
}

// TestManualSendWebhookUnaffected verifies the manual redelivery path keeps
// its synchronous behavior: it records a delivery and creates no queue row.
func TestManualSendWebhookUnaffected(t *testing.T) {
	is := is.New(t)
	usePlainHTTPClient(t)
	ctx, dbx, ds := newTestContext(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hookID := createTestHook(t, ctx, dbx, ds, srv.URL, ContentTypeJSON, true, EventPush)
	w, err := ds.GetWebhookByIDOnly(ctx, dbx, hookID)
	is.NoErr(err)

	is.NoErr(SendWebhook(ctx, w, EventPush, newTestPayload()))

	is.Equal(pendingCount(t, ctx, ds, dbx, hookID), int64(0))
	is.Equal(deliveryCount(t, ctx, ds, dbx, hookID), 1)
}

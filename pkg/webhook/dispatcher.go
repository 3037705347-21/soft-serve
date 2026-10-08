package webhook

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
)

const (
	// dispatchInterval is how often the queue is polled. It also bounds the
	// delivery latency for events enqueued by the short-lived git hook
	// process, which cannot signal the running server directly.
	dispatchInterval = 5 * time.Second
	// claimBatch is the maximum number of deliveries claimed per pass.
	claimBatch = 10
	// dispatchWorkers limits concurrent in-flight HTTP attempts.
	dispatchWorkers = 4
	// claimStaleAfter reclaims in-flight deliveries whose sender died or was
	// restarted before it could finish the delivery.
	claimStaleAfter = 2 * time.Minute
	// maxAttempts bounds retries for a persistently failing receiver.
	// Exhausted deliveries are kept as dead and remain visible together with
	// every attempt's webhook_deliveries record.
	maxAttempts = 12
	// inactiveRetryDelay is how long a queued delivery waits before being
	// reconsidered while its webhook is deactivated. No request is made
	// during that time.
	inactiveRetryDelay = 30 * time.Second
)

// attemptOutcome describes how a claimed delivery attempt ended.
type attemptOutcome int

const (
	outcomeSuccess attemptOutcome = iota
	outcomeRetryable
	outcomeDead
	outcomeCanceled
)

var (
	dispatcherMu   sync.Mutex
	dispatcherWake chan struct{}
)

// notifyDispatcher wakes the in-process dispatcher, if one is running. It is
// a best-effort signal: events enqueued by other processes (git hooks) are
// picked up by the periodic poll.
func notifyDispatcher() {
	dispatcherMu.Lock()
	ch := dispatcherWake
	dispatcherMu.Unlock()

	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Dispatcher reliably delivers durably enqueued webhook events.
type Dispatcher struct {
	ctx    context.Context
	cancel context.CancelFunc
	dbx    *db.DB
	ds     store.Store
	logger *log.Logger
	wake   chan struct{}
	done   chan struct{}

	now func() time.Time
}

// StartDispatcher creates a Dispatcher and starts its background loop.
// Stop must be called to release its resources.
func StartDispatcher(ctx context.Context) *Dispatcher {
	d := NewDispatcher(ctx)
	d.Start()
	return d
}

// NewDispatcher returns a Dispatcher that has not been started. Use
// ProcessOnce to drive it manually (e.g. in tests) or Start to run the
// background loop.
func NewDispatcher(ctx context.Context) *Dispatcher {
	ctx, cancel := context.WithCancel(ctx)
	return &Dispatcher{
		ctx:    ctx,
		cancel: cancel,
		dbx:    db.FromContext(ctx),
		ds:     store.FromContext(ctx),
		logger: log.FromContext(ctx).WithPrefix("webhook.dispatcher"),
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		now:    time.Now,
	}
}

// Start runs the dispatch loop in a goroutine.
func (d *Dispatcher) Start() {
	dispatcherMu.Lock()
	dispatcherWake = d.wake
	dispatcherMu.Unlock()

	go d.loop()
}

// Stop signals the loop to exit and waits for it.
func (d *Dispatcher) Stop() {
	dispatcherMu.Lock()
	if dispatcherWake == d.wake {
		dispatcherWake = nil
	}
	dispatcherMu.Unlock()

	d.cancel()
	<-d.done
}

func (d *Dispatcher) loop() {
	defer close(d.done)

	ticker := time.NewTicker(dispatchInterval)
	defer ticker.Stop()

	// Recover anything left in-flight by a previous process immediately,
	// then react to enqueues and poll periodically for retries.
	d.ProcessOnce(d.ctx)
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.ProcessOnce(d.ctx)
		case <-d.wake:
			d.ProcessOnce(d.ctx)
		}
	}
}

// ProcessOnce claims one batch of due deliveries and attempts them, waiting
// for the batch to finish.
func (d *Dispatcher) ProcessOnce(ctx context.Context) error {
	now := d.now()
	pending, err := d.ds.ClaimDueWebhookPendingDeliveries(ctx, d.dbx, now.Unix(), now.Add(-claimStaleAfter).Unix(), claimBatch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			d.logger.Error("error claiming pending webhook deliveries", "err", err)
		}
		return db.WrapError(err)
	}
	if len(pending) == 0 {
		return nil
	}

	sem := semaphore.NewWeighted(dispatchWorkers)
	var wg sync.WaitGroup
	for _, p := range pending {
		if err := sem.Acquire(ctx, 1); err != nil {
			return err
		}
		wg.Add(1)
		go func(p models.WebhookPendingDelivery) {
			defer wg.Done()
			defer sem.Release(1)
			d.process(ctx, p)
		}(p)
	}
	wg.Wait()
	return nil
}

// process resolves a single claimed delivery.
func (d *Dispatcher) process(ctx context.Context, p models.WebhookPendingDelivery) {
	w, err := d.ds.GetWebhookByIDOnly(ctx, d.dbx, p.WebhookID)
	if err != nil {
		if errors.Is(db.WrapError(err), db.ErrRecordNotFound) {
			// The webhook was removed. The foreign key normally cascades the
			// queue row as well; drop any orphan defensively.
			if err := d.ds.DeleteWebhookPendingDelivery(ctx, d.dbx, p.ID); err != nil {
				d.logger.Error("error deleting orphaned pending delivery", "id", p.ID, "err", err)
			}
			return
		}

		d.logger.Error("error loading webhook for pending delivery", "id", p.ID, "err", err)
		d.reschedule(ctx, p, outcomeRetryable)
		return
	}

	// A deactivated webhook must not generate traffic. The event is held
	// without consuming attempts and goes out automatically if the webhook
	// is reactivated.
	if !w.Active {
		next := d.now().Add(inactiveRetryDelay).Unix()
		if err := d.ds.RequeueWebhookPendingDelivery(ctx, d.dbx, p.ID, p.Attempts, models.WebhookPendingStatusPending, next); err != nil {
			d.logger.Error("error holding delivery for inactive webhook", "id", p.ID, "err", err)
		}
		return
	}

	outcome := d.attempt(ctx, w, p)
	d.reschedule(ctx, p, outcome)
}

// attempt performs one actual HTTP request and records a webhook_deliveries
// row, regardless of outcome, so every real attempt stays auditable.
func (d *Dispatcher) attempt(ctx context.Context, w models.Webhook, p models.WebhookPendingDelivery) attemptOutcome {
	id, err := uuid.NewUUID()
	if err != nil {
		d.logger.Error("error generating delivery id", "id", p.ID, "err", err)
		return outcomeRetryable
	}

	contentType := ContentType(w.ContentType) //nolint:gosec
	headers := buildHeaders(contentType, Event(p.Event), id, p.RequestBody, w.Secret)
	res, reqErr := do(ctx, w.URL, http.MethodPost, headers, bytes.NewBufferString(p.RequestBody))

	resStatus := 0
	resHeaders := ""
	resBody := ""
	if res != nil {
		resStatus, resHeaders, resBody, err = readResponse(res)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return outcomeCanceled
			}
			// Capture the failed body read as the attempt's error, then retry.
			reqErr = err
		}
	}

	if rerr := recordDelivery(ctx, d.ds, id, w, Event(p.Event), reqErr,
		headersString(headers), p.RequestBody, resStatus, resHeaders, resBody); rerr != nil {
		d.logger.Error("error recording webhook delivery attempt", "id", p.ID, "delivery", id, "err", rerr)
	}

	switch {
	case reqErr == nil && resStatus >= 200 && resStatus < 300:
		return outcomeSuccess
	case errors.Is(ctx.Err(), context.Canceled):
		// Shutting down: leave the row to stale-claim recovery on restart.
		return outcomeCanceled
	case reqErr != nil:
		d.logger.Warn("webhook delivery failed, will retry", "webhook", p.WebhookID, "id", p.ID, "attempts", p.Attempts+1, "err", reqErr)
		return outcomeRetryable
	case resStatus == http.StatusTooManyRequests || resStatus >= 500:
		d.logger.Warn("webhook delivery rejected, will retry", "webhook", p.WebhookID, "id", p.ID, "attempts", p.Attempts+1, "status", resStatus)
		return outcomeRetryable
	default:
		// 3xx (redirects are disabled) and non-429 4xx are permanent
		// rejections: don't keep hammering the receiver.
		d.logger.Warn("webhook delivery permanently rejected", "webhook", p.WebhookID, "id", p.ID, "status", resStatus)
		return outcomeDead
	}
}

// reschedule applies the attempt outcome to the queue row.
func (d *Dispatcher) reschedule(ctx context.Context, p models.WebhookPendingDelivery, outcome attemptOutcome) {
	now := d.now().Unix()

	switch outcome {
	case outcomeSuccess:
		if err := d.ds.DeleteWebhookPendingDelivery(ctx, d.dbx, p.ID); err != nil {
			d.logger.Error("error completing pending delivery", "id", p.ID, "err", err)
		}
	case outcomeCanceled:
		// Do not count the interrupted attempt; retry immediately once the
		// process is back. If this write fails because we are shutting down,
		// stale-claim recovery picks the row up on the next start.
		if err := d.ds.RequeueWebhookPendingDelivery(ctx, d.dbx, p.ID, p.Attempts, models.WebhookPendingStatusPending, now); err != nil {
			d.logger.Debug("error requeueing canceled delivery", "id", p.ID, "err", err)
		}
	case outcomeDead:
		if err := d.ds.RequeueWebhookPendingDelivery(ctx, d.dbx, p.ID, p.Attempts+1, models.WebhookPendingStatusDead, now); err != nil {
			d.logger.Error("error marking delivery dead", "id", p.ID, "err", err)
		}
	default:
		attempts := p.Attempts + 1
		if attempts >= maxAttempts {
			d.logger.Warn("webhook delivery exhausted retries", "webhook", p.WebhookID, "id", p.ID, "attempts", attempts)
			if err := d.ds.RequeueWebhookPendingDelivery(ctx, d.dbx, p.ID, attempts, models.WebhookPendingStatusDead, now); err != nil {
				d.logger.Error("error marking delivery dead", "id", p.ID, "err", err)
			}
			return
		}

		next := d.now().Add(retryBackoff(attempts)).Unix()
		if err := d.ds.RequeueWebhookPendingDelivery(ctx, d.dbx, p.ID, attempts, models.WebhookPendingStatusPending, next); err != nil {
			d.logger.Error("error requeueing delivery", "id", p.ID, "err", err)
		}
	}
}

// retryBackoff returns the wait before the given attempt number (1-based):
// 10s, 20s, 40s, ... capped at 5 minutes.
func retryBackoff(attempt int) time.Duration {
	const base = 10 * time.Second
	const capDelay = 5 * time.Minute
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= capDelay {
			return capDelay
		}
	}
	if d > capDelay {
		return capDelay
	}
	return d
}

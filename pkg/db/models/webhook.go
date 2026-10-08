package models

import (
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// Webhook is a repository webhook.
type Webhook struct {
	ID          int64     `db:"id"`
	RepoID      int64     `db:"repo_id"`
	URL         string    `db:"url"`
	Secret      string    `db:"secret"`
	ContentType int       `db:"content_type"`
	Active      bool      `db:"active"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// WebhookEvent is a webhook event.
type WebhookEvent struct {
	ID        int64     `db:"id"`
	WebhookID int64     `db:"webhook_id"`
	Event     int       `db:"event"`
	CreatedAt time.Time `db:"created_at"`
}

// WebhookDelivery is a webhook delivery.
type WebhookDelivery struct {
	ID              uuid.UUID      `db:"id"`
	WebhookID       int64          `db:"webhook_id"`
	Event           int            `db:"event"`
	RequestURL      string         `db:"request_url"`
	RequestMethod   string         `db:"request_method"`
	RequestError    sql.NullString `db:"request_error"`
	RequestHeaders  string         `db:"request_headers"`
	RequestBody     string         `db:"request_body"`
	ResponseStatus  int            `db:"response_status"`
	ResponseHeaders string         `db:"response_headers"`
	ResponseBody    string         `db:"response_body"`
	CreatedAt       time.Time      `db:"created_at"`
}

// Pending delivery statuses.
const (
	WebhookPendingStatusPending  = 0
	WebhookPendingStatusInFlight = 1
	WebhookPendingStatusDead     = 2
)

// WebhookPendingDelivery is a durable, not-yet-completed webhook delivery.
// A row exists only while a delivery is pending or in-flight; it is removed
// once the request succeeds or marked dead after retries are exhausted.
// Every actual HTTP attempt is recorded separately in WebhookDelivery.
type WebhookPendingDelivery struct {
	ID          int64         `db:"id"`
	WebhookID   int64         `db:"webhook_id"`
	Event       int           `db:"event"`
	EventKey    string        `db:"event_key"`
	RequestBody string        `db:"request_body"`
	Status      int           `db:"status"`
	Attempts    int           `db:"attempts"`
	NextRetryAt int64         `db:"next_retry_at"`
	ClaimedAt   sql.NullInt64 `db:"claimed_at"`
	CreatedAt   time.Time     `db:"created_at"`
	UpdatedAt   time.Time     `db:"updated_at"`
}

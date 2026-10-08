package database

import (
	"context"
	"database/sql"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type webhookStore struct{}

var _ store.WebhookStore = (*webhookStore)(nil)

// CreateWebhook implements store.WebhookStore.
func (*webhookStore) CreateWebhook(ctx context.Context, h db.Handler, repoID int64, url string, secret string, contentType int, active bool) (int64, error) {
	var id int64
	query := h.Rebind(`INSERT INTO webhooks (repo_id, url, secret, content_type, active, updated_at)
			VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP) RETURNING id;`)
	err := h.GetContext(ctx, &id, query, repoID, url, secret, contentType, active)
	if err != nil {
		return 0, err
	}

	return id, nil
}

// CreateWebhookDelivery implements store.WebhookStore.
func (*webhookStore) CreateWebhookDelivery(ctx context.Context, h db.Handler, id uuid.UUID, webhookID int64, event int, url string, method string, requestError error, requestHeaders string, requestBody string, responseStatus int, responseHeaders string, responseBody string) error {
	query := h.Rebind(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_error, request_headers, request_body, response_status, response_headers, response_body)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`)
	var reqErr string
	if requestError != nil {
		reqErr = requestError.Error()
	}
	_, err := h.ExecContext(ctx, query, id, webhookID, event, url, method, reqErr, requestHeaders, requestBody, responseStatus, responseHeaders, responseBody)
	return err
}

// CreateWebhookEvents implements store.WebhookStore.
func (*webhookStore) CreateWebhookEvents(ctx context.Context, h db.Handler, webhookID int64, events []int) error {
	query := h.Rebind(`INSERT INTO webhook_events (webhook_id, event)
			VALUES (?, ?);`)
	for _, event := range events {
		_, err := h.ExecContext(ctx, query, webhookID, event)
		if err != nil {
			return err
		}
	}
	return nil
}

// DeleteWebhookByID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookByID(ctx context.Context, h db.Handler, id int64) error {
	query := h.Rebind(`DELETE FROM webhooks WHERE id = ?;`)
	_, err := h.ExecContext(ctx, query, id)
	return err
}

// DeleteWebhookForRepoByID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookForRepoByID(ctx context.Context, h db.Handler, repoID int64, id int64) error {
	query := h.Rebind(`DELETE FROM webhooks WHERE repo_id = ? AND id = ?;`)
	_, err := h.ExecContext(ctx, query, repoID, id)
	return err
}

// DeleteWebhookDeliveryByID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookDeliveryByID(ctx context.Context, h db.Handler, webhookID int64, id uuid.UUID) error {
	query := h.Rebind(`DELETE FROM webhook_deliveries WHERE webhook_id = ? AND id = ?;`)
	_, err := h.ExecContext(ctx, query, webhookID, id)
	return err
}

// DeleteWebhookEventsByWebhookID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookEventsByID(ctx context.Context, h db.Handler, ids []int64) error {
	query, args, err := sqlx.In(`DELETE FROM webhook_events WHERE id IN (?);`, ids)
	if err != nil {
		return err
	}

	query = h.Rebind(query)
	_, err = h.ExecContext(ctx, query, args...)
	return err
}

// GetWebhookByID implements store.WebhookStore.
func (*webhookStore) GetWebhookByID(ctx context.Context, h db.Handler, repoID int64, id int64) (models.Webhook, error) {
	query := h.Rebind(`SELECT * FROM webhooks WHERE repo_id = ? AND id = ?;`)
	var wh models.Webhook
	err := h.GetContext(ctx, &wh, query, repoID, id)
	return wh, err
}

// GetWebhookByIDOnly implements store.WebhookStore.
func (*webhookStore) GetWebhookByIDOnly(ctx context.Context, h db.Handler, id int64) (models.Webhook, error) {
	query := h.Rebind(`SELECT * FROM webhooks WHERE id = ?;`)
	var wh models.Webhook
	err := h.GetContext(ctx, &wh, query, id)
	return wh, err
}

// CreateWebhookPendingDelivery implements store.WebhookStore.
func (*webhookStore) CreateWebhookPendingDelivery(ctx context.Context, h db.Handler, webhookID int64, event int, eventKey string, requestBody string, nextRetryAt int64) error {
	query := h.Rebind(`INSERT INTO webhook_pending_deliveries
			(webhook_id, event, event_key, request_body, status, attempts, next_retry_at)
			VALUES (?, ?, ?, ?, 0, 0, ?)
			ON CONFLICT DO NOTHING;`)
	_, err := h.ExecContext(ctx, query, webhookID, event, eventKey, requestBody, nextRetryAt)
	return err
}

// ClaimDueWebhookPendingDeliveries implements store.WebhookStore.
func (*webhookStore) ClaimDueWebhookPendingDeliveries(ctx context.Context, h db.Handler, now int64, staleBefore int64, limit int) ([]models.WebhookPendingDelivery, error) {
	// Keep the SELECT portable across SQLite/Postgres and do the due/stale
	// filtering in Go: the queue only holds outstanding rows, so this scan
	// stays small. The actual claim is a conditional UPDATE and only the
	// caller that transitions the row (RowsAffected == 1) owns the send.
	query := h.Rebind(`SELECT * FROM webhook_pending_deliveries
			WHERE status IN (?, ?)
			ORDER BY id
			LIMIT ?;`)
	var candidates []models.WebhookPendingDelivery
	if err := h.SelectContext(ctx, &candidates, query,
		models.WebhookPendingStatusPending, models.WebhookPendingStatusInFlight, limit); err != nil {
		return nil, err
	}

	claimed := make([]models.WebhookPendingDelivery, 0)
	claimQuery := h.Rebind(`UPDATE webhook_pending_deliveries
			SET status = ?, claimed_at = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status IN (?, ?);`)
	for _, c := range candidates {
		due := false
		switch c.Status {
		case models.WebhookPendingStatusPending:
			due = c.NextRetryAt <= now
		case models.WebhookPendingStatusInFlight:
			due = c.ClaimedAt.Valid && c.ClaimedAt.Int64 <= staleBefore
		}
		if !due {
			continue
		}

		res, err := h.ExecContext(ctx, claimQuery,
			models.WebhookPendingStatusInFlight, now, c.ID,
			models.WebhookPendingStatusPending, models.WebhookPendingStatusInFlight)
		if err != nil {
			return nil, err
		}

		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			// Another dispatcher claimed the row first.
			continue
		}

		c.Status = models.WebhookPendingStatusInFlight
		c.ClaimedAt = sql.NullInt64{Int64: now, Valid: true}
		claimed = append(claimed, c)
	}

	return claimed, nil
}

// DeleteWebhookPendingDelivery implements store.WebhookStore.
func (*webhookStore) DeleteWebhookPendingDelivery(ctx context.Context, h db.Handler, id int64) error {
	query := h.Rebind(`DELETE FROM webhook_pending_deliveries WHERE id = ?;`)
	_, err := h.ExecContext(ctx, query, id)
	return err
}

// RequeueWebhookPendingDelivery implements store.WebhookStore.
func (*webhookStore) RequeueWebhookPendingDelivery(ctx context.Context, h db.Handler, id int64, attempts int, status int, nextRetryAt int64) error {
	query := h.Rebind(`UPDATE webhook_pending_deliveries
			SET attempts = ?, status = ?, next_retry_at = ?, claimed_at = NULL,
				updated_at = CURRENT_TIMESTAMP
			WHERE id = ?;`)
	_, err := h.ExecContext(ctx, query, attempts, status, nextRetryAt, id)
	return err
}

// CountWebhookPendingDeliveriesByWebhookID implements store.WebhookStore.
func (*webhookStore) CountWebhookPendingDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) (int64, error) {
	query := h.Rebind(`SELECT COUNT(*) FROM webhook_pending_deliveries
			WHERE webhook_id = ? AND status IN (?, ?);`)
	var count int64
	err := h.GetContext(ctx, &count, query,
		webhookID, models.WebhookPendingStatusPending, models.WebhookPendingStatusInFlight)
	return count, err
}

// GetWebhookDeliveriesByWebhookID implements store.WebhookStore.
func (*webhookStore) GetWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookDelivery, error) {
	query := h.Rebind(`SELECT * FROM webhook_deliveries WHERE webhook_id = ?;`)
	var whds []models.WebhookDelivery
	err := h.SelectContext(ctx, &whds, query, webhookID)
	return whds, err
}

// GetWebhookDeliveryByID implements store.WebhookStore.
func (*webhookStore) GetWebhookDeliveryByID(ctx context.Context, h db.Handler, webhookID int64, id uuid.UUID) (models.WebhookDelivery, error) {
	query := h.Rebind(`SELECT * FROM webhook_deliveries WHERE webhook_id = ? AND id = ?;`)
	var whd models.WebhookDelivery
	err := h.GetContext(ctx, &whd, query, webhookID, id)
	return whd, err
}

// GetWebhookEventByID implements store.WebhookStore.
func (*webhookStore) GetWebhookEventByID(ctx context.Context, h db.Handler, id int64) (models.WebhookEvent, error) {
	query := h.Rebind(`SELECT * FROM webhook_events WHERE id = ?;`)
	var whe models.WebhookEvent
	err := h.GetContext(ctx, &whe, query, id)
	return whe, err
}

// GetWebhookEventsByWebhookID implements store.WebhookStore.
func (*webhookStore) GetWebhookEventsByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookEvent, error) {
	query := h.Rebind(`SELECT * FROM webhook_events WHERE webhook_id = ?;`)
	var whes []models.WebhookEvent
	err := h.SelectContext(ctx, &whes, query, webhookID)
	return whes, err
}

// GetWebhooksByRepoID implements store.WebhookStore.
func (*webhookStore) GetWebhooksByRepoID(ctx context.Context, h db.Handler, repoID int64) ([]models.Webhook, error) {
	query := h.Rebind(`SELECT * FROM webhooks WHERE repo_id = ?;`)
	var whs []models.Webhook
	err := h.SelectContext(ctx, &whs, query, repoID)
	return whs, err
}

// GetWebhooksByRepoIDWhereEvent implements store.WebhookStore.
func (*webhookStore) GetWebhooksByRepoIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, events []int) ([]models.Webhook, error) {
	query, args, err := sqlx.In(`SELECT webhooks.*
			FROM webhooks
			INNER JOIN webhook_events ON webhooks.id = webhook_events.webhook_id
			WHERE webhooks.repo_id = ? AND webhooks.active = ? AND webhook_events.event IN (?);`, repoID, true, events)
	if err != nil {
		return nil, err
	}

	query = h.Rebind(query)
	var whs []models.Webhook
	err = h.SelectContext(ctx, &whs, query, args...)
	return whs, err
}

// ListWebhookDeliveriesByWebhookID implements store.WebhookStore.
func (*webhookStore) ListWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookDelivery, error) {
	query := h.Rebind(`SELECT id, response_status, event FROM webhook_deliveries WHERE webhook_id = ?;`)
	var whds []models.WebhookDelivery
	err := h.SelectContext(ctx, &whds, query, webhookID)
	return whds, err
}

// UpdateWebhookByID implements store.WebhookStore.
func (*webhookStore) UpdateWebhookByID(ctx context.Context, h db.Handler, repoID int64, id int64, url string, secret string, contentType int, active bool) error {
	query := h.Rebind(`UPDATE webhooks SET url = ?, secret = ?, content_type = ?, active = ?, updated_at = CURRENT_TIMESTAMP WHERE repo_id = ? AND id = ?;`)
	_, err := h.ExecContext(ctx, query, url, secret, contentType, active, repoID, id)
	return err
}

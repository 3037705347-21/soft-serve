package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/version"
	"github.com/google/go-querystring/query"
	"github.com/google/uuid"
)

// Hook is a repository webhook.
type Hook struct {
	models.Webhook
	ContentType ContentType
	Events      []Event
}

// Delivery is a webhook delivery.
type Delivery struct {
	models.WebhookDelivery
	Event Event
}

// secureHTTPClient is an HTTP client with SSRF protection.
var secureHTTPClient = ssrf.NewSecureClient()

// do sends a webhook.
// Caller must close the returned body.
func do(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header = headers
	res, err := secureHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	return res, nil
}

// encodePayload encodes the event payload according to the webhook content
// type. The returned string is the exact request body that gets sent and
// signed.
func encodePayload(contentType ContentType, payload interface{}) (string, error) {
	var buf bytes.Buffer
	switch contentType {
	case ContentTypeJSON:
		if err := json.NewEncoder(&buf).Encode(payload); err != nil {
			return "", err
		}
	case ContentTypeForm:
		v, err := query.Values(payload)
		if err != nil {
			return "", err
		}
		buf.WriteString(v.Encode()) //nolint: errcheck
	default:
		return "", ErrInvalidContentType
	}

	return buf.String(), nil
}

// buildHeaders builds the request headers for a single delivery attempt.
// A fresh delivery ID and HMAC signature are produced per attempt.
func buildHeaders(contentType ContentType, event Event, id uuid.UUID, body string, secret string) http.Header {
	headers := http.Header{}
	headers.Add("Content-Type", contentType.String())
	headers.Add("User-Agent", "SoftServe/"+version.Version)
	headers.Add("X-SoftServe-Event", event.String())
	headers.Add("X-SoftServe-Delivery", id.String())

	if secret != "" {
		sig := hmac.New(sha256.New, []byte(secret))
		sig.Write([]byte(body)) //nolint: errcheck
		headers.Add("X-SoftServe-Signature", "sha256="+hex.EncodeToString(sig.Sum(nil)))
	}

	return headers
}

func headersString(headers http.Header) string {
	var s string
	for k, v := range headers {
		if len(v) > 0 {
			s += k + ": " + v[0] + "\n"
		}
	}
	return s
}

// readResponse drains the response body and returns the response status,
// headers and body.
func readResponse(res *http.Response) (int, string, string, error) {
	resStatus := res.StatusCode
	var resHeaders string
	for k, v := range res.Header {
		if len(v) > 0 {
			resHeaders += k + ": " + v[0] + "\n"
		}
	}

	var resBody string
	if res.Body != nil {
		defer res.Body.Close() //nolint: errcheck
		b, err := io.ReadAll(res.Body)
		if err != nil {
			return resStatus, resHeaders, "", err
		}
		resBody = string(b)
	}

	return resStatus, resHeaders, resBody, nil
}

// recordDelivery persists the result of a single actual HTTP attempt.
func recordDelivery(ctx context.Context, datastore store.WebhookStore, id uuid.UUID, w models.Webhook, event Event,
	reqErr error, reqHeaders string, reqBody string,
	resStatus int, resHeaders string, resBody string,
) error {
	return db.WrapError(datastore.CreateWebhookDelivery(ctx, db.FromContext(ctx), id, w.ID, int(event), w.URL, http.MethodPost,
		reqErr, reqHeaders, reqBody, resStatus, resHeaders, resBody))
}

// SendWebhook sends a webhook event synchronously and records one delivery.
// This is used for the manual redelivery path; automatic deliveries go
// through the durable queue and Dispatcher.
func SendWebhook(ctx context.Context, w models.Webhook, event Event, payload interface{}) error {
	contentType := ContentType(w.ContentType) //nolint:gosec
	reqBody, err := encodePayload(contentType, payload)
	if err != nil {
		return err
	}

	id, err := uuid.NewUUID()
	if err != nil {
		return err
	}

	headers := buildHeaders(contentType, event, id, reqBody, w.Secret)
	res, reqErr := do(ctx, w.URL, http.MethodPost, headers, bytes.NewBufferString(reqBody))

	resStatus := 0
	resHeaders := ""
	resBody := ""
	if res != nil {
		resStatus, resHeaders, resBody, err = readResponse(res)
		if err != nil {
			return err
		}
	}

	return recordDelivery(ctx, store.FromContext(ctx), id, w, event, reqErr,
		headersString(headers), reqBody, resStatus, resHeaders, resBody)
}

// eventFingerprint returns the deterministic identity of an event payload.
// It is independent of the webhook content type so that JSON and form
// encodings of the same event dedupe together.
func eventFingerprint(event Event, jsonBody []byte) string {
	sum := sha256.New()
	sum.Write([]byte(strconv.Itoa(int(event)))) //nolint: errcheck
	sum.Write([]byte{'\n'})                     //nolint: errcheck
	sum.Write(jsonBody)                         //nolint: errcheck
	return hex.EncodeToString(sum.Sum(nil))
}

// SendEvent durably enqueues a webhook event for every active webhook
// subscribed to it. The actual HTTP delivery happens asynchronously through
// the Dispatcher, which retries failures and recovers across restarts.
//
// The same (webhook, event) payload can only have one outstanding record at
// a time; duplicate enqueues are ignored.
func SendEvent(ctx context.Context, payload EventPayload) error {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	event := payload.Event()

	webhooks, err := datastore.GetWebhooksByRepoIDWhereEvent(ctx, dbx, payload.RepositoryID(), []int{int(event)})
	if err != nil {
		return db.WrapError(err)
	}

	if len(webhooks) == 0 {
		return nil
	}

	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	eventKey := eventFingerprint(event, jsonBody)
	now := time.Now().Unix()

	// Encode and persist every hook's request atomically: once this
	// transaction commits the event survives process restarts.
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		for _, w := range webhooks {
			body, err := encodePayload(ContentType(w.ContentType), payload) //nolint:gosec
			if err != nil {
				return err
			}

			if err := datastore.CreateWebhookPendingDelivery(ctx, tx, w.ID, int(event), eventKey, body, now); err != nil {
				return db.WrapError(err)
			}
		}

		return nil
	}); err != nil {
		return err
	}

	notifyDispatcher()
	return nil
}

func repoURL(publicURL string, repo string) string {
	return fmt.Sprintf("%s/%s.git", publicURL, utils.SanitizeRepo(repo))
}

func getDefaultBranch(repo proto.Repository) (string, error) {
	branch, err := proto.RepositoryDefaultBranch(repo)
	// XXX: we check for ErrReferenceNotExist here because we don't want to
	// return an error if the repo is an empty repo.
	// This means that the repo doesn't have a default branch yet and this is
	// the first push to it.
	if err != nil && !errors.Is(err, git.ErrReferenceNotExist) {
		return "", err
	}

	return branch, nil
}

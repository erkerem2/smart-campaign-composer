package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
)

// Client is the HTTP adapter for dispatch.Sender. It speaks the API served by
// MockServer and maps every failure onto *dispatch.SendError, so the
// dispatcher's retry policy never has to know about HTTP.
type Client struct {
	baseURL string
	http    *http.Client
}

var _ dispatch.Sender = (*Client)(nil)

// NewClient returns a client for the provider at baseURL. A nil httpClient
// gets a client with a 10 s overall timeout; the dispatcher additionally
// bounds every attempt with its own SendTimeout.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

var channelPaths = map[domain.Channel]string{
	domain.ChannelSMS:   "/v1/sms/send",
	domain.ChannelEmail: "/v1/email/send",
}

// Send posts one message with its idempotency key.
func (c *Client) Send(ctx context.Context, m dispatch.Message) (dispatch.Receipt, error) {
	path, ok := channelPaths[m.Channel]
	if !ok {
		return dispatch.Receipt{}, &dispatch.SendError{Err: fmt.Errorf("unsupported channel %q", m.Channel)}
	}
	if m.IdempotencyKey == "" {
		return dispatch.Receipt{}, &dispatch.SendError{Err: errors.New("missing idempotency key")}
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return dispatch.Receipt{}, &dispatch.SendError{Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return dispatch.Receipt{}, &dispatch.SendError{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", m.IdempotencyKey)

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// The caller gave up (shutdown or attempt timeout); report it as
			// is and let the dispatcher decide.
			return dispatch.Receipt{}, ctx.Err()
		}
		// Connection refused/reset, DNS, client timeout: the request may or
		// may not have reached the provider, which is exactly the case the
		// idempotency key makes safe to retry.
		return dispatch.Receipt{}, &dispatch.SendError{Temporary: true, Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ok struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(body, &ok); err != nil || ok.MessageID == "" {
			// Accepted but unreadable: retrying with the same key returns the
			// original id, so this is safe to treat as transient.
			return dispatch.Receipt{}, &dispatch.SendError{StatusCode: resp.StatusCode, Temporary: true,
				Err: errors.New("accepted without a message_id")}
		}
		return dispatch.Receipt{
			ProviderMessageID: ok.MessageID,
			Replayed:          resp.Header.Get("Idempotent-Replayed") == "true",
		}, nil
	}

	se := &dispatch.SendError{
		StatusCode: resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		Temporary:  temporaryStatus(resp.StatusCode),
		Err:        errors.New(errorCode(body, resp.Status)),
	}
	return dispatch.Receipt{}, se
}

// temporaryStatus: 429 and 5xx gateway-style failures may succeed later;
// other 4xx (bad payload, idempotency conflict, auth) never will.
func temporaryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// parseRetryAfter supports both forms of RFC 9110 §10.2.3: delay-seconds and
// HTTP-date. Invalid or negative values yield 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func errorCode(body []byte, fallback string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return fallback
}

// Ping checks the provider's GET /healthz. It is used by the API health
// check; dispatch never depends on it.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("provider unreachable: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provider health: %s", resp.Status)
	}
	return nil
}

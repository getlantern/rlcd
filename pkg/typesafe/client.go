package typesafe

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultURL   = "https://api.typesafe.ai"
	DefaultModel = "jev-latest"

	maxResponse = 8 << 20
	// Retries cover 429, 5xx, and network errors. Asking has no side
	// effects, so a duplicate request can only cost tokens.
	maxRetries    = 2
	backoffBase   = 500 * time.Millisecond
	maxRetryAfter = 30 * time.Second
)

// Client calls the System One API. Empty URL and Model fall back to
// DefaultURL and DefaultModel, and a nil HTTP to http.DefaultClient.
type Client struct {
	Key   string
	URL   string
	Model string
	HTTP  *http.Client
	// LongContext raises the context limits to 262k tokens per request.
	// It is slower and less accurate, so enable it only when needed.
	// Long requests can outlast NewClient's 30s HTTP timeout; raise or
	// clear HTTP.Timeout and bound the call with ctx instead.
	LongContext bool
}

// NewClient returns a client with default URL, model, and a 30s timeout.
func NewClient(key string) *Client {
	return &Client{
		Key:   key,
		URL:   DefaultURL,
		Model: DefaultModel,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError reports a non-2xx response.
type APIError struct {
	Status    int
	Body      string
	RequestID string

	retryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("typesafe: %d %s (request_id=%s): %s",
		e.Status, http.StatusText(e.Status), e.RequestID, e.Body)
}

// Ask evaluates req. An empty req.Model uses the client's model.
// After retries, an API failure is returned as *APIError, even when the
// context ends while waiting to retry.
func (c *Client) Ask(ctx context.Context, req Request) (*Response, error) {
	if c.Key == "" {
		return nil, errors.New("typesafe: missing API key")
	}
	if len(req.Questions) == 0 {
		return nil, errors.New("typesafe: at least one question is required")
	}
	req.Model = cmp.Or(req.Model, c.Model, DefaultModel)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: encoding request: %w", err)
	}

	var out Response
	if err := c.call(ctx, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// call posts body, retrying transient failures,
// and decodes a 2xx response into out.
func (c *Client) call(ctx context.Context, body []byte, out any) error {
	var err error
	for attempt := range maxRetries + 1 {
		err = c.do(ctx, body, out)
		if err == nil || attempt == maxRetries || ctx.Err() != nil {
			return err
		}
		wait, ok := retryDelay(err, attempt)
		if !ok {
			return err
		}
		// Waiting past the deadline would only bury err under a timeout.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			return err
		}
		if serr := sleep(ctx, wait); serr != nil {
			return errors.Join(err, serr)
		}
	}
	return err
}

// retryDelay reports whether err is transient and how long to wait.
// A Retry-After beyond maxRetryAfter is not worth waiting for, so the
// error is returned instead.
func retryDelay(err error, attempt int) (time.Duration, bool) {
	var apiErr *APIError
	var netErr *url.Error
	switch {
	case errors.As(err, &apiErr):
		if apiErr.Status != http.StatusTooManyRequests && apiErr.Status < 500 {
			return 0, false
		}
		if d := apiErr.retryAfter; d > 0 {
			return d, d <= maxRetryAfter
		}
	case errors.As(err, &netErr):
	default:
		return 0, false
	}
	// Jitter to 50-100% of the backoff so parallel callers spread out.
	d := backoffBase << attempt
	return d/2 + rand.N(d/2+1), true
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("typesafe: waiting to retry: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

func (c *Client) do(ctx context.Context, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cmp.Or(c.URL, DefaultURL)+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("typesafe: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.LongContext {
		req.Header.Set("TypeSafe-Danger-Zone-Long-Context", "true")
	}

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("typesafe: %w", err)
	}
	// The body is fully read below, so a close error carries no information.
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("typesafe: reading response: %w", err)
	}
	if len(raw) > maxResponse {
		return fmt.Errorf("typesafe: response exceeds %d bytes", maxResponse)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{
			Status:     resp.StatusCode,
			Body:       truncate(string(raw), 512),
			RequestID:  resp.Header.Get("x-typesafe-request-id"),
			retryAfter: retryAfter(resp.Header),
		}
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("typesafe: decoding response: %w", err)
	}
	return nil
}

// retryAfter parses a Retry-After header in seconds or as an HTTP date.
// It returns 0 when absent, invalid, or already past.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if secs, err := strconv.Atoi(v); err == nil {
		return max(time.Duration(secs)*time.Second, 0)
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Drop any rune split by the cut rather than emit invalid UTF-8.
	return strings.ToValidUTF8(s[:n], "") + "…"
}

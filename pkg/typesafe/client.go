package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	DefaultURL   = "https://api.typesafe.ai"
	DefaultModel = "jev-latest"

	maxResponse = 8 << 20
	// 429 and 529 only, per API docs, to avoid double billing for transport errors.
	maxRetries    = 2
	backoffBase   = 500 * time.Millisecond
	maxRetryAfter = 30 * time.Second
)

// Client calls the System One API.
type Client struct {
	Key   string
	URL   string
	Model string
	HTTP  *http.Client
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
func (c *Client) Ask(ctx context.Context, req Request) (*Response, error) {
	if c.Key == "" {
		return nil, errors.New("typesafe: missing API key")
	}
	if len(req.Questions) == 0 {
		return nil, errors.New("typesafe: at least one question is required")
	}
	if req.Model == "" {
		req.Model = c.Model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: encoding request: %w", err)
	}

	var out Response
	if err := c.call(ctx, http.MethodPost, "/v1/systemone", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Model describes one model available to the account.
type Model struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReleaseDate string `json:"release_date"`
}

// Models lists the model names the account can send in Request.Model.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	if c.Key == "" {
		return nil, errors.New("typesafe: missing API key")
	}
	var out struct {
		Models []Model `json:"models"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/models", nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// call performs one API request, retrying 429 and 529,
// and decodes a 2xx body into out.
func (c *Client) call(ctx context.Context, method, path string, body []byte, out any) error {
	var err error
	for attempt := range maxRetries + 1 {
		err = c.do(ctx, method, path, body, out)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !retryable(apiErr.Status) {
			return err
		}
		if attempt == maxRetries {
			break
		}
		if err := sleep(ctx, apiErr.delay(attempt)); err != nil {
			return err
		}
	}
	return err
}

// delay is how long to wait before retrying after the given attempt.
// It honors Retry-After when within maxRetryAfter.
func (e *APIError) delay(attempt int) time.Duration {
	if e.retryAfter > 0 && e.retryAfter <= maxRetryAfter {
		return e.retryAfter
	}
	return backoffBase << attempt
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

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	url := c.URL + path
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return fmt.Errorf("typesafe: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("typesafe: %s %s: %w", method, url, err)
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

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == 529
}

// retryAfter parses a Retry-After header given in seconds.
func retryAfter(h http.Header) time.Duration {
	secs, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

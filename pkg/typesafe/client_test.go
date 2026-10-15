package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// Example response from https://docs.typesafe.ai/api, all three types.
const docsResponse = `{
  "model": "jev-1.13.0",
  "answers": {
    "is_urgent": {"type": "noul", "noul": 0.95},
    "department": {
      "type": "choice",
      "choice": "billing",
      "probabilities": {"billing": 0.88, "technical": 0.12, "sales": 0.0},
      "confidence": 0.81
    },
    "frustration": {
      "type": "score",
      "score": 1.05,
      "legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
      "probabilities": {"0": 0.0, "1": 0.95, "2": 0.05},
      "confidence": 0.92
    }
  },
  "usage": {"input_tokens": 318, "output_tokens": 34}
}`

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient("ts_test")
	c.URL = srv.URL
	return c
}

func TestAsk(t *testing.T) {
	var got map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer ts_test" {
			t.Errorf("Authorization = %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		write(t, w, docsResponse)
	})

	resp, err := c.Ask(context.Background(), Request{
		State: "Help! My payouts have been failing for 3 days.",
		Questions: map[string]Question{
			"is_urgent": NoulWith("Does this convey urgency?", NoulCriteria{
				True: "Explicitly time-sensitive",
			}),
			"department": Choice("Which team should handle this?", map[string]any{
				"billing":   "Payments, invoicing, refunds",
				"technical": nil,
			}),
			"frustration": Score("How frustrated is the customer?",
				"Calm", "Frustrated", "Very angry"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Request wire shape.
	if got["model"] != DefaultModel {
		t.Errorf("model = %v", got["model"])
	}
	qs := got["questions"].(map[string]any)
	urgent := qs["is_urgent"].(map[string]any)
	if crit := urgent["criteria"].(map[string]any); len(crit) != 1 {
		t.Errorf("noul criteria should omit unset false: %v", crit)
	}
	dept := qs["department"].(map[string]any)["criteria"].(map[string]any)
	if v, ok := dept["technical"]; !ok || v != nil {
		t.Errorf("choice should send null option, got %v (present=%v)", v, ok)
	}
	if lv := qs["frustration"].(map[string]any)["criteria"].([]any); len(lv) != 3 {
		t.Errorf("score levels = %v", lv)
	}
	if _, ok := qs["frustration"].(map[string]any)["type"]; !ok {
		t.Error("question missing type")
	}

	// Response decoding.
	if a := resp.Answers["is_urgent"]; a.Type != TypeNoul || a.Noul != 0.95 {
		t.Errorf("noul = %+v", a)
	}
	if a := resp.Answers["department"]; a.Choice != "billing" ||
		a.Probabilities["technical"] != 0.12 || a.Confidence != 0.81 {
		t.Errorf("choice = %+v", a)
	}
	if a := resp.Answers["frustration"]; a.Score != 1.05 ||
		a.Probabilities["1"] != 0.95 || a.Legend["2"] != "Very angry" {
		t.Errorf("score = %+v", a)
	}
	if resp.Usage.InputTokens != 318 || resp.Model != "jev-1.13.0" {
		t.Errorf("usage/model = %+v %q", resp.Usage, resp.Model)
	}
}

func TestAskRetriesOverload(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(529)
			return
		}
		write(t, w, docsResponse)
	})
	_, err := c.Ask(context.Background(), Request{
		State:     "x",
		Questions: map[string]Question{"q": Noul("?")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
}

func TestAskRetries(t *testing.T) {
	for name, tc := range map[string]struct {
		fail       int // requests that fail before success
		status     int // 0 drops the connection
		retryAfter string
		wantCalls  int32
		wantStatus int // 0 means success
	}{
		"5xx then ok":          {fail: 1, status: 500, wantCalls: 2},
		"network then ok":      {fail: 1, status: 0, wantCalls: 2},
		"5xx gives up":         {fail: 9, status: 503, wantCalls: 3, wantStatus: 503},
		"4xx not retried":      {fail: 9, status: 422, wantCalls: 1, wantStatus: 422},
		"retry-after too long": {fail: 9, status: 429, retryAfter: "120", wantCalls: 1, wantStatus: 429},
		"retry-after honored":  {fail: 1, status: 429, retryAfter: "0", wantCalls: 2},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) > int32(tc.fail) {
					write(t, w, docsResponse)
					return
				}
				if tc.status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Fatal(err)
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Retry-After", tc.retryAfter)
				w.Header().Set("x-typesafe-request-id", "req_1")
				w.WriteHeader(tc.status)
				write(t, w, `{"error":"boom"}`)
			})
			_, err := c.Ask(context.Background(), Request{
				State:     "x",
				Questions: map[string]Question{"q": Noul("?")},
			})
			if n := calls.Load(); n != tc.wantCalls {
				t.Errorf("calls = %d, want %d", n, tc.wantCalls)
			}
			if tc.wantStatus == 0 {
				if err != nil {
					t.Errorf("err = %v", err)
				}
				return
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.wantStatus ||
				apiErr.RequestID != "req_1" {
				t.Errorf("err = %v, want status %d", err, tc.wantStatus)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	for in, want := range map[string]time.Duration{
		"":                              0,
		"5":                             5 * time.Second,
		"-1":                            0,
		"soon":                          0,
		"Wed, 21 Oct 2015 07:28:00 GMT": 0, // past
	} {
		h := http.Header{"Retry-After": {in}}
		if got := retryAfter(h); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", in, got, want)
		}
	}
	got := retryAfter(http.Header{"Retry-After": {future}})
	if got < 8*time.Second || got > 10*time.Second {
		t.Errorf("retryAfter(date +10s) = %v", got)
	}
}

func TestAskValidates(t *testing.T) {
	c := NewClient("")
	if _, err := c.Ask(context.Background(), Request{}); err == nil {
		t.Error("want error for missing key")
	}
	c.Key = "k"
	if _, err := c.Ask(context.Background(), Request{}); err == nil {
		t.Error("want error for no questions")
	}
}

func write(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Errorf("writing response: %v", err)
	}
}

func TestAskLongContextHeader(t *testing.T) {
	for _, on := range []bool{false, true} {
		var got string
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("TypeSafe-Danger-Zone-Long-Context")
			write(t, w, docsResponse)
		})
		c.LongContext = on
		if _, err := c.Ask(context.Background(), Request{
			State:     "x",
			Questions: map[string]Question{"q": Noul("?")},
		}); err != nil {
			t.Fatal(err)
		}
		want := map[bool]string{false: "", true: "true"}[on]
		if got != want {
			t.Errorf("LongContext=%v: header = %q, want %q", on, got, want)
		}
	}
}

// Failures that a retry cannot fix must return after one request.
func TestAskDoesNotRetry(t *testing.T) {
	for name, tc := range map[string]struct {
		handler func(w http.ResponseWriter, cancel context.CancelFunc)
		want    string
	}{
		"cancelled context": {
			handler: func(w http.ResponseWriter, cancel context.CancelFunc) {
				cancel()
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			want: "context canceled",
		},
		"undecodable body": {
			handler: func(w http.ResponseWriter, _ context.CancelFunc) {
				write(t, w, "not json")
			},
			want: "decoding response",
		},
		"oversized body": {
			handler: func(w http.ResponseWriter, _ context.CancelFunc) {
				write(t, w, strings.Repeat("x", maxResponse+1))
			},
			want: "exceeds",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tc.handler(w, cancel)
			})
			_, err := c.Ask(ctx, Request{
				State:     "x",
				Questions: map[string]Question{"q": Noul("?")},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("calls = %d, want 1", n)
			}
		})
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	// "é" is two bytes; cutting at 2 would split the second one.
	got := truncate("aéé", 2)
	if !utf8.ValidString(got) || got != "a…" {
		t.Errorf("truncate = %q", got)
	}
}

// A deadline that ends a retry wait must not hide the API error.
func TestAskKeepsAPIErrorAtDeadline(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "20")
		w.Header().Set("x-typesafe-request-id", "req_1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.Ask(ctx, Request{
		State:     "x",
		Questions: map[string]Question{"q": Noul("?")},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RequestID != "req_1" {
		t.Errorf("err = %v, want the 429 APIError", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("waited for a retry that could not finish before the deadline")
	}
}

func TestZeroClientUsesDefaults(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		write(t, w, docsResponse)
	}))
	t.Cleanup(srv.Close)

	// Only URL is set, to reach the test server; Model and HTTP are zero.
	c := &Client{Key: "k", URL: srv.URL}
	if _, err := c.Ask(context.Background(), Request{
		State:     "x",
		Questions: map[string]Question{"q": Noul("?")},
	}); err != nil {
		t.Fatal(err)
	}
	if got["model"] != DefaultModel {
		t.Errorf("model = %v, want %s", got["model"], DefaultModel)
	}
}

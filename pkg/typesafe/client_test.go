package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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

func TestAskNoRetryOnServerError(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("x-typesafe-request-id", "req_1")
		w.WriteHeader(http.StatusInternalServerError)
		write(t, w, `{"error":"boom"}`)
	})
	_, err := c.Ask(context.Background(), Request{
		State:     "x",
		Questions: map[string]Question{"q": Noul("?")},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != 500 || apiErr.RequestID != "req_1" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1", n)
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

func TestModels(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			t.Errorf("GET should not send Content-Type, got %q", ct)
		}
		write(t, w, `{"models":[{"name":"jev-latest","description":"Flagship","release_date":"2026-09-01"}]}`)
	})
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Name != "jev-latest" || models[0].ReleaseDate != "2026-09-01" {
		t.Errorf("models = %+v", models)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/rlcd/pkg/typesafe"
)

var exampleIDs = []string{
	"is_urgent", "is_spam", "department", "frustration", "is_vip_issue",
}

func TestExampleIsValid(t *testing.T) {
	rf, err := loadRequest("example.toml", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := rf.Problems(); len(p) > 0 {
		t.Fatalf("example.toml invalid: %v", p)
	}
	if !slices.Equal(rf.Order, exampleIDs) {
		t.Errorf("order = %v, want file order %v", rf.Order, exampleIDs)
	}
	crit := rf.Questions["department"].Criteria.(map[string]any)
	if v, ok := crit["other"]; !ok || v != nil {
		t.Errorf(`"" should become null, got %#v`, v)
	}
}

// Dry-run output is the API wire form, which must load back as a
// questions file so requests can be captured and replayed.
func TestDryRunRoundTrips(t *testing.T) {
	var out bytes.Buffer
	a := askCmd{Questions: "example.toml", Text: "hi", StateFormat: "auto",
		Out: "-", Format: "json", DryRun: true}
	if err := a.run(context.Background(), nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	rf, err := loadRequest("-", &out)
	if err != nil {
		t.Fatal(err)
	}
	if p := rf.Problems(); len(p) > 0 {
		t.Fatal(p)
	}
	if rf.State != "hi" || len(rf.Questions) != len(exampleIDs) {
		t.Errorf("round trip = state %v, %d questions", rf.State, len(rf.Questions))
	}
}

func TestReadStatesKeepsAlignment(t *testing.T) {
	in := "{\"a\":1}\nnot json\n\n{\"n\":12345678901234567890}\n"
	var got []string
	for v, err := range readStates(strings.NewReader(in), "json", true) {
		if err != nil {
			got = append(got, "error")
			continue
		}
		b, _ := json.Marshal(v)
		got = append(got, string(b))
	}
	want := []string{`{"a":1}`, "error", "error", `{"n":12345678901234567890}`}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReadStatesWholeText(t *testing.T) {
	var got []any
	for v, err := range readStates(strings.NewReader("line one\nline two\n"), "text", false) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if len(got) != 1 || got[0] != "line one\nline two" {
		t.Errorf("got %q", got)
	}
}

// fakeAPI answers every question with a fixed answer for its type and
// returns 422 when the state is "reject".
func fakeAPI(t *testing.T) (*typesafe.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req typesafe.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		if req.State == "reject" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"detail":"bad"}`)
			return
		}
		answers := map[string]typesafe.Answer{}
		for id, q := range req.Questions {
			a := typesafe.Answer{Type: q.Type}
			switch q.Type {
			case typesafe.TypeNoul:
				a.Noul = 0.9
			case typesafe.TypeChoice:
				a.Choice, a.Confidence = "billing", 0.8
				a.Probabilities = map[string]float64{"billing": 1}
			case typesafe.TypeScore:
				a.Score, a.Confidence = 2.1, 0.7
				a.Legend = map[string]any{"2": "Frustrated"}
				a.Probabilities = map[string]float64{"2": 1}
			}
			answers[id] = a
		}
		_ = json.NewEncoder(w).Encode(typesafe.Response{
			Model: "jev-test", Answers: answers,
		})
	}))
	t.Cleanup(srv.Close)
	c := typesafe.NewClient("k")
	c.URL = srv.URL
	return c, &calls
}

func TestAskLines(t *testing.T) {
	c, calls := fakeAPI(t)
	var out bytes.Buffer
	a := askCmd{Questions: "example.toml", StateFormat: "text", Lines: true,
		Out: "-", Format: "json", Timeout: 5 * time.Second}
	in := strings.NewReader("first\nreject\nthird\n")

	err := a.run(context.Background(), c, in, &out)
	if err == nil || !strings.Contains(err.Error(), "1 of 3 states failed") {
		t.Errorf("err = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 output lines, got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], `"error"`) || !strings.Contains(lines[1], "422") {
		t.Errorf("line 2 = %s", lines[1])
	}
	var resp struct {
		Answers map[string]map[string]any `json:"answers"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &resp); err != nil {
		t.Fatal(err)
	}
	urgent := resp.Answers["is_urgent"]
	if urgent["noul"] != 0.9 || len(urgent) != 2 {
		t.Errorf("noul answer should carry only type and noul: %v", urgent)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("calls = %d", n)
	}
}

func TestAskTable(t *testing.T) {
	c, _ := fakeAPI(t)
	var out bytes.Buffer
	a := askCmd{Questions: "example.toml", Text: "hi", StateFormat: "auto",
		Out: "-", Format: "table", Timeout: 5 * time.Second}
	if err := a.run(context.Background(), c, nil, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range append(exampleIDs, "billing", "2.10 (Frustrated)") {
		if !strings.Contains(s, want) {
			t.Errorf("table missing %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "is_urgent") > strings.Index(s, "is_vip_issue") {
		t.Error("table rows should follow file order")
	}
}

func TestAskUsageErrors(t *testing.T) {
	for name, a := range map[string]askCmd{
		"both stdin": {Questions: "-", State: "-", StateFormat: "auto", Format: "json"},
		"bad format": {Questions: "example.toml", StateFormat: "auto", Format: "yaml"},
		"text+state": {Questions: "example.toml", Text: "x", State: "f", StateFormat: "auto", Format: "json"},
	} {
		t.Run(name, func(t *testing.T) {
			err := a.run(context.Background(), nil, strings.NewReader(""), io.Discard)
			var usage *usageError
			if !errors.As(err, &usage) {
				t.Errorf("err = %v, want usageError", err)
			}
		})
	}
}

func TestNewClientEnv(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "k")

	t.Setenv("TYPESAFE_BASE_URL", "")
	c, err := newClient()
	if err != nil || c.URL != typesafe.DefaultURL {
		t.Errorf("default: url %q, err %v", c.URL, err)
	}

	t.Setenv("TYPESAFE_BASE_URL", "https://openrouter.ai/api/")
	c, err = newClient()
	if err != nil || c.URL != "https://openrouter.ai/api" {
		t.Errorf("override: url %q, err %v", c.URL, err)
	}

	for _, bad := range []string{"http://openrouter.ai/api", "openrouter.ai/api", "https://"} {
		t.Setenv("TYPESAFE_BASE_URL", bad)
		var usage *usageError
		if _, err := newClient(); !errors.As(err, &usage) {
			t.Errorf("%q: err = %v, want usageError", bad, err)
		}
	}

	t.Setenv("TYPESAFE_API_KEY", "")
	if _, err := newClient(); err == nil {
		t.Error("want error for missing key")
	}
}

func TestAskLongContextFlag(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("TypeSafe-Danger-Zone-Long-Context")
		_, _ = io.WriteString(w, `{"model":"m","answers":{}}`)
	}))
	t.Cleanup(srv.Close)
	c := typesafe.NewClient("k")
	c.URL = srv.URL

	a := askCmd{Questions: "example.toml", Text: "hi", StateFormat: "auto",
		Out: "-", Format: "json", Timeout: 5 * time.Second, LongContext: true}
	if err := a.run(context.Background(), c, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got != "true" {
		t.Errorf("long-context header = %q, want true", got)
	}
}

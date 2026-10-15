package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	qf, err := loadQuestions("example.toml", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := qf.Problems(); len(p) > 0 {
		t.Fatalf("example.toml invalid: %v", p)
	}
	if !slices.Equal(qf.Order, exampleIDs) {
		t.Errorf("order = %v, want file order %v", qf.Order, exampleIDs)
	}
	crit := qf.Questions["department"].Criteria.(map[string]any)
	if v, ok := crit["other"]; !ok || v != nil {
		t.Errorf(`"" should become null, got %#v`, v)
	}
}

// Dry-run output is the API wire form, which must load back as a
// questions file so requests can be captured and replayed.
func TestDryRunRoundTrips(t *testing.T) {
	var out bytes.Buffer
	a := askCmd{Questions: "example.toml", Text: "hi", StateFormat: "auto",
		Out: "-", Format: "json", DryRun: true, Timeout: time.Second}
	if err := a.run(context.Background(), nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	qf, err := loadQuestions("-", &out)
	if err != nil {
		t.Fatal(err)
	}
	if p := qf.Problems(); len(p) > 0 {
		t.Fatal(p)
	}
	if qf.State != "hi" || len(qf.Questions) != len(exampleIDs) {
		t.Errorf("round trip = state %v, %d questions", qf.State, len(qf.Questions))
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
		"both stdin":        {Questions: "-", State: "-", StateFormat: "auto", Format: "json"},
		"bad format":        {Questions: "example.toml", StateFormat: "auto", Format: "yaml"},
		"text+state":        {Questions: "example.toml", Text: "x", State: "f", StateFormat: "auto", Format: "json"},
		"zero timeout":      {Questions: "example.toml", Text: "x", StateFormat: "auto", Format: "json"},
		"no questions file": {Questions: "missing.toml", Text: "x", StateFormat: "auto", Format: "json", Timeout: time.Second},
		"no state file":     {Questions: "example.toml", State: "missing.txt", StateFormat: "auto", Format: "json", Timeout: time.Second, DryRun: true},
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

// testdata/docs holds the request bodies published at docs.typesafe.ai.
// Each must load as a questions file and come back from --dry-run
// unchanged, so examples from the docs work as-is.
func TestDocsRequestsRoundTrip(t *testing.T) {
	paths, err := filepath.Glob("testdata/docs/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no testdata: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var out bytes.Buffer
			a := askCmd{Questions: path, StateFormat: "auto", Out: "-",
				Format: "json", DryRun: true, Timeout: time.Second}
			if err := a.run(context.Background(), nil, nil, &out); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			// Some doc snippets omit the required model; ask fills in
			// the default, and nothing else may change.
			if w := want.(map[string]any); w["model"] == nil {
				w["model"] = typesafe.DefaultModel
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip changed the request\n got: %s\nwant: %s",
					out.Bytes(), raw)
			}
		})
	}
}

func TestCreateOutputIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A failed run leaves the previous file and no temp files.
	w, commit, err := createOutput(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, w, "partial\n")
	if err := commit(false); err != nil {
		t.Fatal(err)
	}
	assertDir(t, dir, path, "old\n")

	// A successful run replaces it whole.
	w, commit, err = createOutput(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, w, "new\n")
	if err := commit(true); err != nil {
		t.Fatal(err)
	}
	assertDir(t, dir, path, "new\n")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 (err %v)", fi.Mode().Perm(), err)
	}
}

// assertDir checks path holds want and is the only file in dir.
func assertDir(t *testing.T, dir, path, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1 (temp file left behind?)", len(entries))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func TestCheck(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.toml")
	err := os.WriteFile(bad, []byte("[[questions]]\nid='q'\ntype='score'\ninstructions='x'\ncriteria=['a']\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&checkCmd{Files: []string{"example.toml"}}).run(context.Background(), nil); err != nil {
		t.Errorf("valid file: %v", err)
	}
	err = (&checkCmd{Files: []string{"example.toml", bad}}).run(context.Background(), nil)
	var usage *usageError
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("invalid file: err = %v, want usageError for 1 of 2", err)
	}
}

// stdout carries only results; every log line, including per-state
// failures, must go to the logger's writer.
func TestLogsStayOffStdout(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(newLogger(&logs, true))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c, _ := fakeAPI(t)
	var out bytes.Buffer
	a := askCmd{Questions: "example.toml", StateFormat: "text", Lines: true,
		Out: "-", Format: "json", Timeout: 5 * time.Second}
	_ = a.run(context.Background(), c, strings.NewReader("ok\nreject\n"), &out)

	for i, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("stdout line %d is not JSON: %q", i+1, line)
		}
	}
	if !strings.Contains(logs.String(), "state failed") {
		t.Errorf("failure was not logged to the logger's writer:\n%s", logs.String())
	}
}

func write(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Errorf("writing: %v", err)
	}
}

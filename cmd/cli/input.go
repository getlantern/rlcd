package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/getlantern/rlcd/pkg/typesafe"
)

// requestFile is a parsed questions file. Model and State are optional.
type requestFile struct {
	Model     string
	State     any
	Questions map[string]typesafe.Question
	// Order is the question ids in file order, for table output.
	Order []string
}

// readInput reads path, or stdin when path is "-".
func readInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}

// loadRequest parses a questions file in TOML or JSON. A document
// starting with '{' is JSON; TOML cannot start with an inline table.
func loadRequest(path string, stdin io.Reader) (*requestFile, error) {
	raw, err := readInput(path, stdin)
	if err != nil {
		return nil, fmt.Errorf("reading questions: %w", err)
	}
	var doc map[string]any
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, usagef("%s: parsing JSON: %v", path, err)
		}
	} else if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, usagef("%s: parsing TOML: %v", path, err)
	}
	rf, err := parseRequest(doc)
	if err != nil {
		return nil, usagef("%s: %v", path, err)
	}
	return rf, nil
}

// parseRequest accepts questions as an array of tables with an id
// (the file form) or as a map keyed by id (the API wire form).
func parseRequest(doc map[string]any) (*requestFile, error) {
	rf := &requestFile{Questions: map[string]typesafe.Question{}}
	for key, val := range doc {
		switch key {
		case "model":
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("model must be a string")
			}
			rf.Model = s
		case "state":
			rf.State = val
		case "questions":
		default:
			return nil, fmt.Errorf("unknown top-level key %q", key)
		}
	}

	add := func(id string, fields map[string]any) error {
		if id == "" {
			return errors.New("question is missing an id")
		}
		if _, dup := rf.Questions[id]; dup {
			return fmt.Errorf("duplicate question id %q", id)
		}
		q, err := parseQuestion(fields)
		if err != nil {
			return fmt.Errorf("question %q: %w", id, err)
		}
		rf.Questions[id] = q
		rf.Order = append(rf.Order, id)
		return nil
	}

	switch qs := doc["questions"].(type) {
	case nil:
		return nil, errors.New("no questions")
	case []map[string]any: // TOML array of tables
		for i, fields := range qs {
			id, _ := fields["id"].(string)
			if err := add(id, fields); err != nil {
				return nil, fmt.Errorf("questions[%d]: %w", i, err)
			}
		}
	case []any: // JSON array
		for i, item := range qs {
			fields, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("questions[%d] must be an object", i)
			}
			id, _ := fields["id"].(string)
			if err := add(id, fields); err != nil {
				return nil, fmt.Errorf("questions[%d]: %w", i, err)
			}
		}
	case map[string]any:
		for _, id := range slices.Sorted(maps.Keys(qs)) {
			fields, ok := qs[id].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("question %q must be a table", id)
			}
			if err := add(id, fields); err != nil {
				return nil, err
			}
		}
	default:
		return nil, errors.New("questions must be an array or a table")
	}
	return rf, nil
}

// parseQuestion rejects unknown keys so typos fail before an API call.
// TOML has no null, so an empty-string Choice description means none.
func parseQuestion(fields map[string]any) (typesafe.Question, error) {
	var q typesafe.Question
	for key, val := range fields {
		switch key {
		case "id":
		case "type":
			s, _ := val.(string)
			q.Type = typesafe.TsType(s)
		case "instructions":
			q.Instructions = val
		case "criteria":
			q.Criteria = val
		default:
			return q, fmt.Errorf("unknown key %q", key)
		}
	}
	if opts, ok := q.Criteria.(map[string]any); ok && q.Type == typesafe.TypeChoice {
		for name, desc := range opts {
			if desc == "" {
				opts[name] = nil
			}
		}
	}
	return q, nil
}

// validate reports problems the API would reject, in file order.
func validate(rf *requestFile) []string {
	var problems []string
	for _, id := range rf.Order {
		q := rf.Questions[id]
		bad := func(format string, args ...any) {
			problems = append(problems, id+": "+fmt.Sprintf(format, args...))
		}
		if q.Instructions == nil || q.Instructions == "" {
			bad("missing instructions")
		}
		switch q.Type {
		case typesafe.TypeNoul:
			if q.Criteria == nil {
				break
			}
			crit, ok := q.Criteria.(map[string]any)
			if !ok {
				bad("noul criteria must be a table with true and/or false")
				break
			}
			for k := range crit {
				if k != "true" && k != "false" {
					bad("noul criteria has unknown key %q", k)
				}
			}
		case typesafe.TypeChoice:
			crit, ok := q.Criteria.(map[string]any)
			if !ok {
				bad("choice criteria must be a table of option = description")
				break
			}
			if n := len(crit); n < 1 || n > 255 {
				bad("choice needs 1 to 255 options, has %d", n)
			}
		case typesafe.TypeScore:
			n, ok := listLen(q.Criteria)
			if !ok {
				bad("score criteria must be an array of level descriptions")
				break
			}
			if n < 2 || n > 10 {
				bad("score needs 2 to 10 levels, has %d", n)
			}
		case "":
			bad("missing type")
		default:
			bad("unknown type %q; use noul, choice, or score", q.Type)
		}
	}
	return problems
}

// listLen handles []any from JSON and typed slices from TOML.
func listLen(v any) (int, bool) {
	switch l := v.(type) {
	case []any:
		return len(l), true
	case []string:
		return len(l), true
	case []map[string]any:
		return len(l), true
	}
	return 0, false
}

// stateFormat resolves "auto" from the file extension; stdin is text.
func stateFormat(format, path string) string {
	if format != "auto" {
		return format
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".jsonl", ".ndjson":
		return "json"
	}
	return "text"
}

// maxLine bounds one state in --lines mode; the API caps bodies at 32 MiB.
const maxLine = 32 << 20

// readStates yields each state from r: the whole input as one state,
// or one per line. A bad line yields an error but does not stop the
// sequence, so output lines stay aligned with input lines.
func readStates(r io.Reader, format string, lines bool) iter.Seq2[any, error] {
	return func(yield func(any, error) bool) {
		if !lines {
			raw, err := io.ReadAll(r)
			if err != nil {
				yield(nil, fmt.Errorf("reading state: %w", err))
				return
			}
			yield(parseState(raw, format, true))
			return
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			if !yield(parseState(sc.Bytes(), format, false)) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(nil, fmt.Errorf("reading states: %w", err))
		}
	}
}

func parseState(raw []byte, format string, whole bool) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("empty state")
	}
	if format == "text" {
		s := string(raw)
		if whole {
			// Drop the newline echo and heredocs add.
			s = strings.TrimRight(s, "\r\n")
		}
		return s, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Keep large integers exact; the API rejects lossy numbers.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parsing JSON state: %w", err)
	}
	if dec.More() {
		return nil, errors.New("JSON state has trailing data; use --lines for JSONL")
	}
	return v, nil
}

// createOutput returns stdout for "-", or a temp file that commit
// renames over path so a failed run never leaves partial output.
func createOutput(path string, stdout io.Writer) (io.Writer, func(ok bool) error, error) {
	if path == "-" {
		return stdout, func(bool) error { return nil }, nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating output: %w", err)
	}
	commit := func(ok bool) error {
		closeErr := f.Close()
		if !ok || closeErr != nil {
			_ = os.Remove(f.Name())
			if closeErr != nil {
				return fmt.Errorf("writing output: %w", closeErr)
			}
			return nil
		}
		if err := os.Rename(f.Name(), path); err != nil {
			_ = os.Remove(f.Name())
			return fmt.Errorf("writing output: %w", err)
		}
		return nil
	}
	return f, commit, nil
}

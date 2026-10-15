// Package questions parses TypeSafe questions files: TOML or JSON
// documents holding typed questions plus an optional model and state.
//
// Files use an array of tables with an id; the API wire form, a map
// keyed by id, is accepted too so captured requests can be replayed.
package questions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/BurntSushi/toml"

	"github.com/getlantern/rlcd/pkg/typesafe"
)

// File is a parsed questions file. Model and State are optional.
type File struct {
	Model     string
	State     any
	Questions map[string]typesafe.Question
	// Order is the question ids in file order.
	Order []string
}

// Parse reads a questions file in TOML or JSON. A document starting
// with '{' is JSON; TOML cannot start with an inline table.
func Parse(raw []byte) (*File, error) {
	var doc map[string]any
	trimmed := bytes.TrimSpace(raw)
	isTOML := len(trimmed) == 0 || trimmed[0] != '{'
	if !isTOML {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("parsing JSON: %w", err)
		}
	} else if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing TOML: %w", err)
	}
	return parseDoc(doc, isTOML)
}

func parseDoc(doc map[string]any, isTOML bool) (*File, error) {
	f := &File{Questions: map[string]typesafe.Question{}}
	for key, val := range doc {
		switch key {
		case "model":
			s, ok := val.(string)
			if !ok {
				return nil, errors.New("model must be a string")
			}
			f.Model = s
		case "state":
			f.State = val
		case "questions":
		default:
			return nil, fmt.Errorf("unknown top-level key %q", key)
		}
	}

	add := func(id string, fields map[string]any) error {
		if id == "" {
			return errors.New("question is missing an id")
		}
		if _, dup := f.Questions[id]; dup {
			return fmt.Errorf("duplicate question id %q", id)
		}
		q, err := parseQuestion(fields, isTOML)
		if err != nil {
			return fmt.Errorf("question %q: %w", id, err)
		}
		f.Questions[id] = q
		f.Order = append(f.Order, id)
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
	return f, nil
}

// parseQuestion rejects unknown keys so typos fail before an API call.
// TOML has no null, so there an empty-string Choice description means
// none. JSON can say null itself, so its values pass through unchanged.
func parseQuestion(fields map[string]any, isTOML bool) (typesafe.Question, error) {
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
	if opts, ok := q.Criteria.(map[string]any); ok && isTOML && q.Type == typesafe.TypeChoice {
		for name, desc := range opts {
			if desc == "" {
				opts[name] = nil
			}
		}
	}
	return q, nil
}

// Problems reports what the API would reject, in file order.
func (f *File) Problems() []string {
	var problems []string
	for _, id := range f.Order {
		q := f.Questions[id]
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

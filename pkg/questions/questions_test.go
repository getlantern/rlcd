package questions

import (
	"strings"
	"testing"
)

func TestProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		toml string
		want string
	}{
		"typo key":     {`instrutions = "x"`, `unknown key "instrutions"`},
		"one level":    {"type='score'\ninstructions='x'\ncriteria=['a']", "2 to 10 levels"},
		"bad type":     {"type='bool'\ninstructions='x'", `unknown type "bool"`},
		"no criteria":  {"type='choice'\ninstructions='x'", "choice criteria"},
		"no instr":     {"type='noul'", "missing instructions"},
		"noul bad key": {"type='noul'\ninstructions='x'\ncriteria.yes='y'", `unknown key "yes"`},
	} {
		t.Run(name, func(t *testing.T) {
			doc := "[[questions]]\nid = 'q'\n" + tc.toml + "\n"
			f, err := Parse([]byte(doc))
			var got string
			switch {
			case err != nil:
				got = err.Error()
			default:
				got = strings.Join(f.Problems(), "; ")
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestParseWireForm(t *testing.T) {
	doc := `{"state": "hi", "questions": {
		"b": {"type": "noul", "instructions": "x"},
		"a": {"type": "noul", "instructions": "y"}}}`
	f, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if f.State != "hi" || strings.Join(f.Order, ",") != "a,b" {
		t.Errorf("got state %v, order %v", f.State, f.Order)
	}
}

func TestParseRejectsUnknownTopLevel(t *testing.T) {
	_, err := Parse([]byte("modle = 'x'\n[[questions]]\nid='q'\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown top-level key "modle"`) {
		t.Errorf("got %v", err)
	}
}

// Only TOML needs "" as a stand-in for null; JSON passes through as sent.
func TestChoiceEmptyDescription(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		want any
	}{
		"toml": {"[[questions]]\nid='q'\ntype='choice'\ninstructions='x'\ncriteria.other=''\n", nil},
		"json": {`{"questions": {"q": {"type": "choice", "instructions": "x", "criteria": {"other": ""}}}}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := Parse([]byte(tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			got := f.Questions["q"].Criteria.(map[string]any)["other"]
			if got != tc.want {
				t.Errorf("other = %#v, want %#v", got, tc.want)
			}
		})
	}
}

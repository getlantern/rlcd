package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/pterm/pterm"

	"github.com/getlantern/rlcd/pkg/questions"
	"github.com/getlantern/rlcd/pkg/typesafe"
)

type askCmd struct {
	Questions   string        `arg:"-q,--questions,required" help:"questions file, TOML or JSON; - for stdin"`
	State       string        `arg:"-s,--state" help:"state file; - for stdin [default: the file's state, else stdin]"`
	Text        string        `arg:"--text" help:"inline text state"`
	StateFormat string        `arg:"-S,--state-format" default:"auto" help:"auto, text, or json"`
	Lines       bool          `arg:"-l,--lines" help:"treat each input line as its own state"`
	Model       string        `arg:"-m,--model" help:"model, overriding the questions file [default: jev-latest]"`
	Out         string        `arg:"-o,--out" default:"-" help:"output file; - for stdout"`
	Format      string        `arg:"-f,--format" default:"json" help:"json or table"`
	DryRun      bool          `arg:"--dry-run" help:"write request bodies instead of calling the API"`
	Timeout     time.Duration `arg:"--timeout" default:"60s" help:"limit per state, including retries"`
	LongContext bool          `arg:"--long-context" help:"raise context limits to 262k tokens; slower and less accurate"`
}

// run writes one result per state, in input order. A failed state
// writes {"error": ...} in its place and the run continues.
func (a *askCmd) run(ctx context.Context, c *typesafe.Client, stdin io.Reader, stdout io.Writer) error {
	if a.StateFormat != "auto" && a.StateFormat != "text" && a.StateFormat != "json" {
		return usagef("--state-format must be auto, text, or json")
	}
	if a.Format != "json" && a.Format != "table" {
		return usagef("--format must be json or table")
	}
	if a.Text != "" && a.State != "" {
		return usagef("--text and --state are mutually exclusive")
	}
	if a.Questions == "-" && a.State == "-" {
		return usagef("questions and state cannot both come from stdin")
	}
	if a.Timeout <= 0 {
		return usagef("--timeout must be positive, got %s", a.Timeout)
	}

	qf, err := loadQuestions(a.Questions, stdin)
	if err != nil {
		return err
	}
	if problems := qf.Problems(); len(problems) > 0 {
		for _, p := range problems {
			slog.ErrorContext(ctx, "invalid question",
				"file", a.Questions,
				"problem", p,
			)
		}
		return usagef("%s: %d invalid question(s)", a.Questions, len(problems))
	}

	if c != nil {
		c.LongContext = a.LongContext
	}
	model := cmp.Or(a.Model, qf.Model, typesafe.DefaultModel)
	states, err := a.states(qf, stdin)
	if err != nil {
		return err
	}

	if a.Format == "table" && a.Out == "-" && !isTerminal(stdout) {
		pterm.DisableStyling()
	}
	w, commit, err := createOutput(a.Out, stdout)
	if err != nil {
		return err
	}

	var total, failed int
	for state, err := range states {
		if ctx.Err() != nil {
			_ = commit(false)
			return fmt.Errorf("interrupted after %d states: %w", total, ctx.Err())
		}
		total++
		req := typesafe.Request{State: state, Model: model, Questions: qf.Questions}
		var resp *typesafe.Response
		if err == nil {
			resp, err = a.ask(ctx, c, req)
		}
		if err != nil {
			failed++
			slog.ErrorContext(ctx, "state failed",
				"state", total,
				"error", err,
			)
		}
		if werr := a.write(w, qf.Order, req, resp, err); werr != nil {
			_ = commit(false)
			return fmt.Errorf("writing output: %w", werr)
		}
	}
	// A Ctrl-C during the last state lands here rather than at the top
	// of the loop; report it as an interrupt, not as failed states.
	if ctx.Err() != nil {
		_ = commit(false)
		return fmt.Errorf("interrupted after %d states: %w", total, ctx.Err())
	}
	if err := commit(true); err != nil {
		return err
	}
	slog.DebugContext(ctx, "done",
		"states", total,
		"failed", failed,
	)
	if failed > 0 {
		return fmt.Errorf("%d of %d states failed", failed, total)
	}
	return nil
}

// states picks the state source: --text, then --state, then the
// questions file's own state, then stdin.
func (a *askCmd) states(qf *questions.File, stdin io.Reader) (iter.Seq2[any, error], error) {
	if a.Text != "" {
		return func(yield func(any, error) bool) { yield(a.Text, nil) }, nil
	}
	path := a.State
	if path == "" && qf.State != nil {
		return func(yield func(any, error) bool) { yield(qf.State, nil) }, nil
	}
	if path == "" {
		path = "-"
	}
	if path == "-" && a.Questions == "-" {
		return nil, usagef("questions and state cannot both come from stdin")
	}
	format := stateFormat(a.StateFormat, path)
	if path == "-" {
		return readStates(stdin, format, a.Lines), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, usagef("opening state: %w", err)
	}
	seq := readStates(f, format, a.Lines)
	return func(yield func(any, error) bool) {
		defer func() { _ = f.Close() }()
		seq(yield)
	}, nil
}

func (a *askCmd) ask(ctx context.Context, c *typesafe.Client, req typesafe.Request) (*typesafe.Response, error) {
	if a.DryRun {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	return c.Ask(ctx, req)
}

func (a *askCmd) write(w io.Writer, order []string, req typesafe.Request, resp *typesafe.Response, err error) error {
	if a.Format == "table" && !a.DryRun {
		return writeTable(w, order, resp, err)
	}
	var v any = resp
	switch {
	case err != nil:
		v = map[string]string{"error": err.Error()}
	case a.DryRun:
		v = req
	}
	b, merr := json.Marshal(v)
	if merr != nil {
		return merr
	}
	_, werr := w.Write(append(b, '\n'))
	return werr
}

func writeTable(w io.Writer, order []string, resp *typesafe.Response, err error) error {
	if err != nil {
		_, werr := fmt.Fprintf(w, "error: %v\n\n", err)
		return werr
	}
	data := pterm.TableData{{"ID", "TYPE", "ANSWER", "CONF"}}
	for _, id := range order {
		ans := resp.Answers[id]
		answer, conf := "", "-"
		switch ans.Type {
		case typesafe.TypeNoul:
			answer = fmt.Sprintf("%.2f", ans.Noul)
		case typesafe.TypeChoice:
			answer, conf = ans.Choice, fmt.Sprintf("%.2f", ans.Confidence)
		case typesafe.TypeScore:
			level := strconv.Itoa(int(math.Round(ans.Score)))
			answer = fmt.Sprintf("%.2f (%v)", ans.Score, ans.Legend[level])
			conf = fmt.Sprintf("%.2f", ans.Confidence)
		}
		data = append(data, []string{id, string(ans.Type), answer, conf})
	}
	s, err := pterm.DefaultTable.WithHasHeader().WithData(data).Srender()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, s+"\n")
	return err
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Command rlcd asks TypeSafe System One questions about states read
// from files or stdin, writing one JSON line per state.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"

	"github.com/alexflint/go-arg"
	"github.com/pterm/pterm"

	"github.com/getlantern/rlcd/pkg/typesafe"
)

var version = "dev"

//go:embed example.toml
var exampleTOML []byte

type cli struct {
	Ask     *askCmd     `arg:"subcommand:ask" help:"evaluate states against questions"`
	Check   *checkCmd   `arg:"subcommand:check" help:"validate questions files offline"`
	Example *exampleCmd `arg:"subcommand:example" help:"print an example questions file"`

	Verbose bool `arg:"-v,--verbose" help:"debug logging to stderr"`
}

func (cli) Version() string { return "rlcd " + version }

type exampleCmd struct{}

// usageError reports bad flags or an invalid questions file; exit code 2.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// usagef formats like fmt.Errorf, including %w, as a usageError.
func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

func main() {
	var args cli
	p := arg.MustParse(&args)

	if !isTerminal(os.Stderr) {
		pterm.DisableColor()
	}
	slog.SetDefault(newLogger(os.Stderr, args.Verbose))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnInterrupt(ctx, cancel)

	err := run(ctx, &args, os.Stdin, os.Stdout)
	var usage *usageError
	switch {
	case err == nil:
	case p.Subcommand() == nil:
		p.WriteHelp(os.Stderr)
		os.Exit(2)
	case errors.As(err, &usage):
		slog.ErrorContext(ctx, "invalid usage", "error", err)
		os.Exit(2)
	case errors.Is(err, context.Canceled):
		slog.ErrorContext(ctx, "interrupted", "error", err)
		os.Exit(130)
	default:
		slog.ErrorContext(ctx, "run failed", "error", err)
		os.Exit(1)
	}
}

// newLogger logs through pterm to w. pterm writes to stdout by default,
// which would corrupt piped output, so the writer is always explicit.
func newLogger(w io.Writer, verbose bool) *slog.Logger {
	level := pterm.LogLevelInfo
	if verbose {
		level = pterm.LogLevelDebug
	}
	logger := pterm.DefaultLogger.WithWriter(w).WithLevel(level)
	return slog.New(pterm.NewSlogHandler(logger))
}

// cancelOnInterrupt cancels ctx on the first Ctrl-C so in-flight work
// stops cleanly, then restores the default handler. Reading stdin does
// not watch ctx, so a second Ctrl-C must still be able to exit.
func cancelOnInterrupt(ctx context.Context, cancel context.CancelFunc) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		// Reset restores the default handler: a second Ctrl-C exits.
		defer signal.Reset(os.Interrupt)
		select {
		case <-sig:
			slog.WarnContext(ctx, "interrupted; press Ctrl-C again to exit now")
			cancel()
		case <-ctx.Done():
		}
	}()
}

func run(ctx context.Context, args *cli, stdin io.Reader, stdout io.Writer) error {
	switch {
	case args.Ask != nil:
		var c *typesafe.Client
		if !args.Ask.DryRun {
			var err error
			if c, err = newClient(); err != nil {
				return err
			}
		}
		return args.Ask.run(ctx, c, stdin, stdout)
	case args.Check != nil:
		return args.Check.run(ctx, stdin)
	case args.Example != nil:
		_, err := stdout.Write(exampleTOML)
		return err
	}
	return usagef("a subcommand is required")
}

// newClient configures a client from the environment only. There are
// deliberately no flags, so the key never lands in shell history or ps.
//
//   - TYPESAFE_API_KEY: required.
//   - TYPESAFE_BASE_URL: optional API root, e.g. https://openrouter.ai/api
//     to use an OpenRouter key. Must be https so the key is never sent
//     in the clear.
func newClient() (*typesafe.Client, error) {
	key := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY"))
	if key == "" {
		return nil, usagef("set TYPESAFE_API_KEY")
	}
	c := typesafe.NewClient(key)
	// ask bounds each state with --timeout. A fixed per-attempt timeout
	// would cut off long-context requests and retry them pointlessly.
	c.HTTP.Timeout = 0
	if raw := strings.TrimSpace(os.Getenv("TYPESAFE_BASE_URL")); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, usagef("TYPESAFE_BASE_URL must be an https URL, got %q", raw)
		}
		c.URL = strings.TrimRight(raw, "/")
	}
	return c, nil
}

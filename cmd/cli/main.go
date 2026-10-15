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
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

func main() {
	var args cli
	p := arg.MustParse(&args)

	level := pterm.LogLevelInfo
	if args.Verbose {
		level = pterm.LogLevelDebug
	}
	if !isTerminal(os.Stderr) {
		pterm.DisableColor()
	}
	// pterm logs to stdout by default, which would corrupt piped output.
	logger := pterm.DefaultLogger.WithWriter(os.Stderr).WithLevel(level)
	slog.SetDefault(slog.New(pterm.NewSlogHandler(logger)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err := run(ctx, &args, os.Stdin, os.Stdout)
	stop()
	var usage *usageError
	switch {
	case err == nil:
	case p.Subcommand() == nil:
		p.WriteHelp(os.Stderr)
		os.Exit(2)
	case errors.As(err, &usage):
		slog.ErrorContext(ctx, "invalid usage", "error", err)
		os.Exit(2)
	default:
		slog.ErrorContext(ctx, "run failed", "error", err)
		os.Exit(1)
	}
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
	if raw := strings.TrimSpace(os.Getenv("TYPESAFE_BASE_URL")); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, usagef("TYPESAFE_BASE_URL must be an https URL, got %q", raw)
		}
		c.URL = strings.TrimRight(raw, "/")
	}
	return c, nil
}

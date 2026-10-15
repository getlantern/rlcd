package main

import (
	"context"
	"io"
	"log/slog"
)

type checkCmd struct {
	Files []string `arg:"positional,required" help:"questions files; - for stdin"`
}

// run validates every file without calling the API, logging each
// problem, so CI can gate changes to question files.
func (c *checkCmd) run(ctx context.Context, stdin io.Reader) error {
	var bad int
	for _, path := range c.Files {
		rf, err := loadRequest(path, stdin)
		if err != nil {
			slog.ErrorContext(ctx, "invalid file", "error", err)
			bad++
			continue
		}
		problems := rf.Problems()
		for _, p := range problems {
			slog.ErrorContext(ctx, "invalid question", "file", path, "problem", p)
		}
		if len(problems) > 0 {
			bad++
			continue
		}
		slog.DebugContext(ctx, "ok", "file", path, "questions", len(rf.Order))
	}
	if bad > 0 {
		return usagef("%d of %d files invalid", bad, len(c.Files))
	}
	return nil
}

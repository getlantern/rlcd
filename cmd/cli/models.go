package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/pterm/pterm"

	"github.com/getlantern/rlcd/pkg/typesafe"
)

type modelsCmd struct {
	Format string `arg:"-f,--format" default:"json" help:"json or table"`
}

// run writes one JSON line per model, or a table.
func (m *modelsCmd) run(ctx context.Context, c *typesafe.Client, stdout io.Writer) error {
	if m.Format != "json" && m.Format != "table" {
		return usagef("--format must be json or table")
	}
	models, err := c.Models(ctx)
	if err != nil {
		return fmt.Errorf("listing models: %w", err)
	}
	if m.Format == "table" {
		if !isTerminal(stdout) {
			pterm.DisableStyling()
		}
		data := pterm.TableData{{"NAME", "RELEASED", "DESCRIPTION"}}
		for _, md := range models {
			data = append(data, []string{md.Name, md.ReleaseDate, md.Description})
		}
		s, err := pterm.DefaultTable.WithHasHeader().WithData(data).Srender()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, s)
		return err
	}
	enc := json.NewEncoder(stdout)
	for _, md := range models {
		if err := enc.Encode(md); err != nil {
			return err
		}
	}
	return nil
}

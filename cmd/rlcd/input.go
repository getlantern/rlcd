package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"github.com/getlantern/rlcd/pkg/questions"
)

// readInput reads path, or stdin when path is "-".
func readInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}

// loadQuestions parses a questions file in TOML or JSON.
func loadQuestions(path string, stdin io.Reader) (*questions.File, error) {
	raw, err := readInput(path, stdin)
	if err != nil {
		return nil, usagef("reading questions: %w", err)
	}
	f, err := questions.Parse(raw)
	if err != nil {
		return nil, usagef("%s: %w", path, err)
	}
	return f, nil
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
		// CreateTemp makes files 0600; give the output the usual mode.
		err := f.Chmod(0o644)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if ok && err == nil {
			err = os.Rename(f.Name(), path)
		}
		if !ok || err != nil {
			_ = os.Remove(f.Name())
		}
		if err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
		return nil
	}
	return f, commit, nil
}

// Command pricetable writes the embedded price table snapshot from a local
// copy of LiteLLM's model_prices_and_context_window.json. It is a
// maintainer tool and never enters the shipped binary.
//
// Usage:
//
//	go run ./tools/pricetable -source <path> -revision <commit> -revision-time <time> [-out internal/pricing/snapshot]
//
// The source is the file at
// https://raw.githubusercontent.com/BerriAI/litellm/<revision>/model_prices_and_context_window.json,
// downloaded beforehand: the tool makes no network request. The revision
// time is the commit time of the revision as RFC 3339, as
// "git show -s --format=%cI <revision>" prints it.
//
// The snapshot, REVISION, and REVISION_TIME are written together, so a
// revision always names the rates beside it, and only after every input
// has passed validation.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/sortie-ai/sortie/internal/pricing"
)

const defaultOut = "internal/pricing/snapshot"

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pricetable:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("pricetable", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	source := fs.String("source", "", "path of the upstream model_prices_and_context_window.json")
	revision := fs.String("revision", "", "upstream commit of the source, 40 lowercase hex characters")
	revisionTime := fs.String("revision-time", "", "commit time of the revision, RFC 3339")
	out := fs.String("out", defaultOut, "directory the snapshot files are written to")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w (flags: -source, -revision, -revision-time, -out)", err)
	}

	required := []struct{ name, value string }{
		{"source", *source},
		{"revision", *revision},
		{"revision-time", *revisionTime},
	}
	for _, f := range required {
		if f.value == "" {
			return fmt.Errorf("missing flag -%s", f.name)
		}
	}
	if !revisionPattern.MatchString(*revision) {
		return errors.New("revision is not 40 lowercase hex characters")
	}
	commitTime, err := time.Parse(time.RFC3339, *revisionTime)
	if err != nil {
		return fmt.Errorf("revision time: %w", err)
	}

	raw, err := os.ReadFile(*source)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	table, err := pricing.ParseTable(raw, nil)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}

	files := []struct {
		name string
		body []byte
	}{
		{"model_prices_and_context_window.json", table.Encode()},
		{"REVISION", []byte(*revision + "\n")},
		{"REVISION_TIME", []byte(commitTime.UTC().Format(time.RFC3339) + "\n")},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(*out, f.name), f.body, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}

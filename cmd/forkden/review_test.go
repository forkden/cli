package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	v1 "github.com/forkden/cli/api/v1"
)

func TestReviewExitCodes(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]int{"review_partial": 4, "check_failed": 3, "invalid_input": 2, "unsupported_source": 2, "dump_failed": 1} {
		if got := exitCode(&v1.Error{Code: code}); got != want {
			t.Errorf("exitCode(%s) = %d; want %d", code, got, want)
		}
	}
}

func TestReviewRejectsTheEngineSocket(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := run(t.Context(), []string{"--json", "--socket", "/tmp/engine.sock", "review", "diff", "demo"}, &out)
	if exitCode(err) != 2 || !strings.Contains(out.String(), "review connects to PostgreSQL directly") {
		t.Fatalf("review --socket = %q, %v", out.String(), err)
	}
}

func TestReviewKeepsTheNameInFrontOfFlags(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"fork", "diff", "check", "export", "drop"} {
		cmd, consumed, err := resolveCommand([]string{"review", sub})
		if err != nil || cmd.path != "review "+sub {
			t.Fatalf("resolve review %s = %q, %v", sub, cmd.path, err)
		}
		if leadingPositionals(cmd.path) != 1 {
			t.Errorf("review %s must keep one leading positional", sub)
		}
		ordered := orderCommandArgs(cmd, []string{"review", sub, "--to", "postgres://x/y", "demo"}, consumed)
		if !slices.Equal(ordered, []string{"review", sub, "demo", "--to", "postgres://x/y"}) {
			t.Errorf("review %s order = %v", sub, ordered)
		}
	}
	if cmd, _, _ := resolveCommand([]string{"cloud", "review"}); cmd.path != "cloud review" {
		t.Errorf("cloud review must stay the cloud command, got %q", cmd.path)
	}
}

func TestReviewInputErrorsNeverConnect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FORKDEN_REVIEW_DIR", dir)
	for _, args := range [][]string{
		{"review", "fork", "demo"},
		{"review", "fork", "Bad-Name", "--dsn", "postgres://localhost/x"},
		{"review", "diff", "demo", "--to", "postgres://localhost/x", "--base", "postgres://a/b", "--work", "postgres://a/c"},
		{"review", "diff", "demo", "--base", "postgres://a/b"},
		{"review", "check", "demo"},
		{"review", "export", "demo"},
		{"review", "drop", "--to", "postgres://localhost/x"},
	} {
		var out bytes.Buffer
		err := run(t.Context(), append([]string{"--json"}, args...), &out)
		var envelope map[string]any
		if exitCode(err) != 2 || json.Unmarshal(out.Bytes(), &envelope) != nil || envelope["ok"] != false {
			t.Errorf("run(%v) = %q, %v; want a JSON invalid_input with exit 2", args, out.String(), err)
		}
	}
}

func TestReviewHelpListsEverySubcommand(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := run(t.Context(), []string{"help"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Review:\n  review") || !strings.Contains(out.String(), "Get started (review)") {
		t.Errorf("root help lacks the Review group:\n%s", out.String())
	}
	for _, sub := range []string{"fork", "diff", "check", "export", "drop"} {
		out.Reset()
		if err := run(t.Context(), []string{"help", "review", sub}, &out); err != nil || !strings.Contains(out.String(), "Usage: forkden review "+sub+" NAME") {
			t.Errorf("help review %s = %q, %v", sub, out.String(), err)
		}
	}
}

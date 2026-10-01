// Command forkden provides the open-source client for the Forkden engine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	v1 "github.com/forkden/cli/api/v1"
)

var version = "0.1.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if code := exitCode(err); code != 0 {
		os.Exit(code)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := commandFlags("forkden")
	compact := flags.Bool("json", false, "compact JSON envelope; place before command")
	socket := flags.String("socket", os.Getenv("FORKDEN_ENGINE_SOCKET"), "engine Unix socket path (or FORKDEN_ENGINE_SOCKET)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(out, usage)
			return errors.Join(err, writeErr)
		}
		inputErr := invalidInput(err.Error())
		return errors.Join(inputErr, writeOutput(out, nil, inputErr, *compact))
	}
	args = flags.Args()
	if len(args) == 0 || args[0] == "help" {
		_, err := fmt.Fprint(out, usage)
		return err
	}
	if args[0] == "version" {
		return writeOutput(out, map[string]any{"version": version, "api_version": v1.Version, "os": runtime.GOOS, "arch": runtime.GOARCH}, nil, *compact)
	}
	if (args[0] == "db" || args[0] == "source") && (len(args) == 1 || args[1] == "help" || args[1] == "--help") {
		_, err := fmt.Fprint(out, profileUsage)
		return err
	}
	if *socket == "" {
		home := os.Getenv("FORKDEN_HOME")
		if home == "" {
			configDir, err := os.UserConfigDir()
			if err != nil {
				return errors.Join(err, writeOutput(out, nil, err, *compact))
			}
			home = filepath.Join(configDir, "forkden")
		}
		*socket = filepath.Join(home, "engine.sock")
	}
	path, err := filepath.Abs(*socket)
	if err != nil {
		return errors.Join(err, writeOutput(out, nil, err, *compact))
	}
	client, err := v1.NewClient(path)
	if err != nil {
		return errors.Join(err, writeOutput(out, nil, err, *compact))
	}
	result, err := dispatch(ctx, client, args, out)
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return errors.Join(err, writeOutput(out, result, err, *compact))
}

func writeOutput(out io.Writer, value any, operationErr error, compact bool) error {
	envelope := map[string]any{"ok": operationErr == nil, "data": value}
	if operationErr != nil {
		publicErr := &v1.Error{Code: "client_error", Message: operationErr.Error()}
		var engineErr *v1.Error
		if errors.As(operationErr, &engineErr) {
			publicErr = engineErr
		}
		envelope["error"] = publicErr
	}
	encoder := json.NewEncoder(out)
	if !compact {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(envelope)
}

func exitCode(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var publicErr *v1.Error
	if errors.As(err, &publicErr) {
		switch publicErr.Code {
		case "invalid_input", "unsupported_action", "unsupported_version":
			return 2
		case "check_failed":
			return 3
		}
	}
	return 1
}

func invalidInput(message string) error {
	return &v1.Error{Code: "invalid_input", Message: message}
}

const usage = `Forkden · open-source client for the private engine

Usage: forkden [--json] [--socket PATH] COMMAND

  db add NAME --url_env ENV              Register an engine-side DB reference
  db list / db show NAME                 Read profile metadata
  db check NAME                         Verify connection and pinned database
  db update NAME --url_env ENV           Rotate the reference for the same DB
  db remove NAME                        Remove a profile after closing its forks
  fork create --source NAME [--ttl 1h]   Create an isolated database fork
  fork list / fork close ID / fork prune
  query --fork ID --file FILE            Run one SQL statement on the fork
  check --fork ID --file FILE            Read-only SELECT returning one boolean
  diff --fork ID                        Review changes
  history --fork ID                     Inspect operation outcomes
  export --fork ID --output DIR          Write SQL and report on this machine
  engine status                         Show engine/protocol readiness
  version                               Show CLI/protocol version

source is an alias for db. query/check also accept --sql.
Global flags precede commands. Start forkden-engine serve separately.
DB variables must be available to the ENGINE process.
Exit codes: 0 success; 1 operation error; 2 invalid input; 3 false check.
`

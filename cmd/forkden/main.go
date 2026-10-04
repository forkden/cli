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

	"github.com/forkden/cli/api/cloud"
	v1 "github.com/forkden/cli/api/v1"
)

var version = "0.4.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if code := exitCode(err); code != 0 {
		os.Exit(code)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	return runWithCloud(ctx, args, out, os.Stderr, defaultCloudApp())
}

func runWithCloud(ctx context.Context, args []string, out, progress io.Writer, app cloudApp) error {
	flags := commandFlags("forkden")
	compact := flags.Bool("json", false, "compact JSON envelope; place before command")
	socket := flags.String("socket", os.Getenv("FORKDEN_ENGINE_SOCKET"), "engine Unix socket path (or FORKDEN_ENGINE_SOCKET)")
	apiURL := flags.String("api_url", os.Getenv("FORKDEN_API_URL"), "account API origin; saved after login")
	configDir := flags.String("config_dir", os.Getenv("FORKDEN_CLI_CONFIG_DIR"), "private CLI settings directory")
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
	if args[0] == "cloud" || args[0] == "login" || args[0] == "logout" || args[0] == "whoami" {
		cloudArgs := args
		if args[0] == "cloud" {
			cloudArgs = args[1:]
		}
		if *configDir == "" {
			dir, err := os.UserConfigDir()
			if err != nil {
				return errors.Join(err, writeOutput(out, nil, err, *compact))
			}
			*configDir = filepath.Join(dir, "forkden", "cli")
		}
		var socketExplicit bool
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "socket" {
				socketExplicit = true
			}
		})
		if socketExplicit {
			err := invalidInput("--socket belongs to local engine commands; cloud commands use --api_url")
			return errors.Join(err, writeOutput(out, nil, err, *compact))
		}
		result, err := app.run(ctx, cloudArgs, *apiURL, *configDir, out, progress)
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errors.Join(err, writeOutput(out, result, err, *compact))
	}
	if (args[0] == "db" || args[0] == "source") && (len(args) == 1 || args[1] == "help" || args[1] == "--help") {
		_, err := fmt.Fprint(out, profileUsage)
		return err
	}
	if args[0] == "clone" && (len(args) == 1 || args[1] == "help" || args[1] == "--help") {
		_, err := fmt.Fprint(out, cloneUsage)
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
		var cloudErr *cloud.Error
		if errors.As(operationErr, &cloudErr) {
			publicErr = &v1.Error{Code: cloudErr.Code, Message: cloudErr.Message}
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
	var cloudErr *cloud.Error
	if errors.As(err, &cloudErr) && cloudErr.Code == "invalid_input" {
		return 2
	}
	return 1
}

func invalidInput(message string) error {
	return &v1.Error{Code: "invalid_input", Message: message}
}

const usage = `Forkden · open-source client for the private engine

Usage: forkden [--json] [--socket PATH] COMMAND

  login / whoami / logout               Account access with browser consent
  cloud help                            Workspace selection and copy jobs

Account flags: --api_url ORIGIN, --config_dir DIR (before the command).
The cloud prefix submits metadata jobs to the account API for either deployment mode.
The commands below use the local Unix socket development protocol.

  db add NAME --url_env ENV              Register an engine-side DB reference
  db list / db show NAME                 Read profile metadata
  db check NAME                         Verify connection and pinned database
  db update NAME --url_env ENV           Rotate the reference for the same DB
  db remove NAME                        Remove a profile after closing its forks
  clone create NAME --source PROFILE     Capture immutable snapshot v1
  clone list / clone show NAME_OR_ID
  clone refresh / clone remove NAME_OR_ID
  clone prune                           Recover interrupted snapshot cleanup
  fork create --clone NAME [--version N] Fork a fixed snapshot without reading source
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

// Command forkden provides the open-source client for the Forkden engine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/forkden/cli/api/cloud"
	v1 "github.com/forkden/cli/api/v1"
)

var version = "0.5.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	stop()
	if code := exitCode(err); code != 0 {
		os.Exit(code)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	return runWithCloud(ctx, args, out, os.Stderr, defaultCloudApp())
}

func runWithCloud(ctx context.Context, args []string, out, progress io.Writer, app cloudApp) error {
	opts, args, err := parseGlobalOptions(args)
	output := newOutput(out, progress, opts)
	if err != nil {
		return output.finish(nil, err)
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		args = []string{"version"}
	}
	if len(args) > 0 && args[0] == "help" {
		cmd, err := validateHelpPath(args[1:])
		output.command = cmd.path
		if err != nil {
			return output.finish(nil, err)
		}
		return writeHelp(out, cmd)
	}
	cmd, consumed, err := resolveCommand(args)
	output.command = cmd.path
	if err != nil {
		return output.finish(nil, err)
	}
	remaining := args[consumed:]
	groupHelp := len(cmd.children) > 0 && (len(remaining) == 0 || (len(remaining) == 1 && remaining[0] == "help"))
	if groupHelp || wantsHelp(remaining) {
		return writeHelp(out, cmd)
	}
	if cmd.path == "" || len(cmd.children) > 0 {
		return output.finish(nil, invalidInput("unknown flag or incomplete command; run forkden help"))
	}
	args = orderCommandArgs(cmd, args, consumed)
	if cmd.path == "completion" {
		if len(args) != 2 {
			return output.finish(nil, invalidInput("use forkden completion bash or zsh"))
		}
		script, err := completionScript(args[1])
		if err != nil {
			return output.finish(nil, err)
		}
		_, err = io.WriteString(out, script)
		return err
	}
	if args[0] == "version" {
		if len(args) != 1 {
			return output.finish(nil, invalidInput("version takes no arguments"))
		}
		return output.finish(map[string]any{"version": version, "api_version": v1.Version, "os": runtime.GOOS, "arch": runtime.GOARCH}, nil)
	}
	if args[0] == "cloud" || args[0] == "login" || args[0] == "logout" || args[0] == "whoami" {
		cloudArgs := args
		if args[0] == "cloud" {
			cloudArgs = args[1:]
		}
		if opts.configDir == "" {
			dir, err := os.UserConfigDir()
			if err != nil {
				return output.finish(nil, err)
			}
			opts.configDir = filepath.Join(dir, "forkden", "cli")
		}
		if opts.localOnly {
			return output.finish(nil, invalidInput("--socket belongs to local engine commands; cloud commands use --api-url"))
		}
		result, err := app.run(ctx, cloudArgs, opts.apiURL, opts.configDir, out, progress)
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return output.finish(result, err)
	}
	if opts.socket == "" {
		home := os.Getenv("FORKDEN_HOME")
		if home == "" {
			configDir, err := os.UserConfigDir()
			if err != nil {
				return output.finish(nil, err)
			}
			home = filepath.Join(configDir, "forkden")
		}
		opts.socket = filepath.Join(home, "engine.sock")
	}
	path, err := filepath.Abs(opts.socket)
	if err != nil {
		return output.finish(nil, err)
	}
	client, err := v1.NewClient(path)
	if err != nil {
		return output.finish(nil, err)
	}
	result, err := dispatch(ctx, client, args, out)
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return output.finish(result, err)
}

func writeOutput(out io.Writer, value any, operationErr error, compact bool) error {
	envelope := map[string]any{"ok": operationErr == nil, "data": value}
	if operationErr != nil {
		envelope["error"] = publicError(operationErr)
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
	if errors.Is(err, context.Canceled) {
		return 130
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
	if errors.As(err, &cloudErr) {
		switch cloudErr.Code {
		case "invalid_input":
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

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

type globalOptions struct {
	compact   bool
	format    string
	socket    string
	apiURL    string
	configDir string
	localOnly bool
}

var valueFlags = map[string]bool{
	"api_url": true, "config_dir": true, "socket": true, "format": true,
	"url_env": true, "target_env": true, "source": true, "clone": true,
	"version": true, "ttl": true, "fork": true, "sql": true, "file": true,
	"output": true, "profile": true, "timeout": true, "request_id": true,
	"output_dir": true, "org": true,
}

func parseGlobalOptions(args []string) (globalOptions, []string, error) {
	opts := globalOptions{
		format:    "auto",
		socket:    os.Getenv("FORKDEN_ENGINE_SOCKET"),
		apiURL:    os.Getenv("FORKDEN_API_URL"),
		configDir: os.Getenv("FORKDEN_CLI_CONFIG_DIR"),
	}
	flags := commandFlags("forkden")
	flags.BoolVar(&opts.compact, "json", false, "compact JSON output")
	flags.StringVar(&opts.format, "format", opts.format, "auto, table or json")
	flags.StringVar(&opts.socket, "socket", opts.socket, "local engine socket")
	flags.StringVar(&opts.apiURL, "api_url", opts.apiURL, "account API origin")
	flags.StringVar(&opts.configDir, "config_dir", opts.configDir, "CLI settings directory")
	globalArgs := make([]string, 0, len(args))
	commandArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			commandArgs = append(commandArgs, args[i:]...)
			break
		}
		name, inline := flagName(arg)
		if name == "version" && len(commandArgs) == 0 && !inline {
			commandArgs = append(commandArgs, "version")
			continue
		}
		if name == "" {
			commandArgs = append(commandArgs, arg)
			continue
		}
		target := &commandArgs
		if flags.Lookup(name) != nil {
			target = &globalArgs
		}
		*target = append(*target, normalizeFlag(arg))
		if !inline && valueFlags[name] && i+1 < len(args) {
			i++
			*target = append(*target, args[i])
		}
	}
	if err := flags.Parse(globalArgs); err != nil {
		return opts, commandArgs, invalidInput(err.Error())
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "socket" {
			opts.localOnly = true
		}
	})
	if opts.format != "auto" && opts.format != "json" && opts.format != "table" {
		return opts, commandArgs, invalidInput("--format must be auto, table or json")
	}
	if opts.compact && opts.format == "table" {
		return opts, commandArgs, invalidInput("--json and --format table cannot be combined")
	}
	return opts, commandArgs, nil
}

func flagName(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "-") || arg == "-" {
		return "", false
	}
	name, _, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
	return strings.ReplaceAll(name, "-", "_"), inline
}

func normalizeFlag(arg string) string {
	name, inline := flagName(arg)
	if !valueFlags[name] && name != "allow_rows" {
		return arg
	}
	if inline {
		_, value, _ := strings.Cut(arg, "=")
		return "--" + name + "=" + value
	}
	return "--" + name
}

func wantsHelp(args []string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return false
		}
		if args[i] == "--help" || args[i] == "-h" {
			return true
		}
		name, inline := flagName(args[i])
		if valueFlags[name] && !inline {
			i++
		}
	}
	return false
}

// Handlers historically expect named resources before flags, except SQL and
// session commands. Partitioning at the boundary accepts either ordering while
// preserving every flag value (including SQL that happens to start with "--").
func orderCommandArgs(cmd commandHelp, args []string, consumed int) []string {
	options := make([]string, 0, len(args)-consumed)
	positionals := make([]string, 0, len(args)-consumed)
	for i := consumed; i < len(args); i++ {
		if args[i] == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		name, inline := flagName(args[i])
		if name == "" {
			positionals = append(positionals, args[i])
			continue
		}
		options = append(options, args[i])
		if valueFlags[name] && !inline && i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	result := append([]string{}, args[:consumed]...)
	if cmd.path == "cloud session create" || cmd.path == "cloud query" || cmd.path == "cloud check" {
		result = append(result, options...)
		if len(positionals) > 0 {
			result = append(result, "--")
		}
		return append(result, positionals...)
	}
	count := min(leadingPositionals(cmd.path), len(positionals))
	result = append(result, positionals[:count]...)
	result = append(result, options...)
	if len(positionals) > count {
		result = append(result, "--")
		result = append(result, positionals[count:]...)
	}
	return result
}

func leadingPositionals(path string) int {
	switch path {
	case "db add", "db update", "db show", "db check", "db remove",
		"clone create", "clone show", "clone refresh", "clone remove", "fork close",
		"cloud project use", "cloud clone create", "cloud clone show", "cloud clone versions", "cloud clone refresh", "cloud clone delete",
		"cloud fork create", "cloud fork show", "cloud fork close", "cloud job status", "cloud job wait", "cloud job cancel", "completion":
		return 1
	default:
		return 0
	}
}

func validateHelpPath(args []string) (commandHelp, error) {
	cmd, consumed, err := resolveCommand(args)
	if err != nil {
		return cmd, err
	}
	if consumed != len(args) {
		return cmd, invalidInput(fmt.Sprintf("unexpected help argument %q", args[consumed]))
	}
	return cmd, nil
}

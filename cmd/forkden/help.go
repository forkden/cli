package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

// The catalog keeps offline help, command validation and shell completion in sync.
// Execution stays in the existing local and cloud command handlers.
type commandHelp struct {
	path     string
	summary  string
	usage    string
	example  string
	flags    string
	children []commandHelp
}

var commands = []commandHelp{
	{path: "login", summary: "Sign in through your browser", usage: "[--no-browser]", example: "forkden login --api-url https://app.tryforkden.com", flags: "--no-browser\tPrint the authorization URL without opening a browser"},
	{path: "whoami", summary: "Show your signed-in account"},
	{path: "logout", summary: "Revoke your account token and sign out"},
	{path: "cloud", summary: "Work with your account, projects and private workers", children: []commandHelp{
		{path: "cloud org", summary: "List your workspaces", children: []commandHelp{
			{path: "cloud org list", summary: "List workspace memberships"},
		}},
		{path: "cloud project", summary: "Select a project", children: []commandHelp{
			{path: "cloud project list", summary: "List projects in a workspace", usage: "--org ORG_ID"},
			{path: "cloud project use", summary: "Save the active workspace and project", usage: "PROJECT_ID --org ORG_ID", example: "forkden cloud project use prj_example --org org_example"},
			{path: "cloud project current", summary: "Show the selected API, workspace and project"},
		}},
		{path: "cloud db", summary: "Inspect worker-linked DB profiles", children: []commandHelp{
			{path: "cloud db list", summary: "List profile metadata for the selected project"},
		}},
		{path: "cloud clone", summary: "Capture and manage immutable snapshots", children: []commandHelp{
			{path: "cloud clone create", summary: "Capture a snapshot through the assigned worker", usage: "NAME --profile PROFILE_ID [--wait]", flags: jobFlags, example: "forkden cloud clone create commerce --profile prf_example --wait"},
			{path: "cloud clone list", summary: "List clones in the selected project"},
			{path: "cloud clone show", summary: "Inspect clone lifecycle metadata", usage: "RESOURCE_ID"},
			{path: "cloud clone versions", summary: "List immutable snapshot versions", usage: "RESOURCE_ID"},
			{path: "cloud clone refresh", summary: "Publish a new snapshot version", usage: "RESOURCE_ID [--wait]", flags: jobFlags},
			{path: "cloud clone delete", summary: "Request clone removal on the worker", usage: "RESOURCE_ID [--wait]", flags: jobFlags},
		}},
		{path: "cloud fork", summary: "Create and manage isolated experiments", children: []commandHelp{
			{path: "cloud fork create", summary: "Fork a ready snapshot version", usage: "NAME --clone RESOURCE_ID [--version 1] [--ttl 1h] [--wait]", flags: "--version N\tReady snapshot version (default 1)\n--ttl DURATION\tFork lifetime, 1m–24h (default 1h)\n" + jobFlags, example: "forkden cloud fork create backfill --clone res_example --version 1 --wait"},
			{path: "cloud fork list", summary: "List forks in the selected project"},
			{path: "cloud fork show", summary: "Inspect fork lifecycle metadata", usage: "RESOURCE_ID"},
			{path: "cloud fork close", summary: "Request fork cleanup on the worker", usage: "RESOURCE_ID [--wait]", flags: jobFlags},
		}},
		{path: "cloud job", summary: "Inspect, wait for or cancel worker jobs", children: []commandHelp{
			{path: "cloud job list", summary: "List job history"},
			{path: "cloud job status", summary: "Show status and cleanup state", usage: "JOB_ID"},
			{path: "cloud job wait", summary: "Follow a job until it finishes", usage: "JOB_ID [--timeout 20m]", flags: "--timeout DURATION\tPolling limit, 1s–1h (default 20m); stopping does not cancel the job"},
			{path: "cloud job cancel", summary: "Request cancellation and worker cleanup", usage: "JOB_ID"},
		}},
		{path: "cloud session", summary: "Manage a short-lived, fork-bound SQL session", children: []commandHelp{
			{path: "cloud session create", summary: "Select a fork for private work", usage: "FORK_ID [--ttl 10m] [--allow-rows]", flags: "--ttl DURATION\tSession lifetime, 30s–10m (default 10m)\n--allow-rows\tExplicitly allow transient result rows and review samples", example: "forkden cloud session create res_example --ttl 5m"},
			{path: "cloud session show", summary: "Inspect the selected work session"},
			{path: "cloud session revoke", summary: "Revoke the selected work session"},
		}},
		{path: "cloud query", summary: "Run SQL inside the selected fork session", usage: "(SQL | --file FILE | --sql SQL) [--request-id ID]", flags: cloudSQLFlags, example: "forkden cloud query --file backfill.sql"},
		{path: "cloud check", summary: "Evaluate a read-only boolean check", usage: "(SQL | --file FILE | --sql SQL) [--request-id ID]", flags: cloudSQLFlags},
		{path: "cloud schema", summary: "Inspect the fork schema", flags: workFlags},
		{path: "cloud history", summary: "Inspect private operation history", flags: workFlags},
		{path: "cloud review", summary: "Review row changes and checks", flags: workFlags},
		{path: "cloud export", summary: "Write SQL and a report to a new local directory", usage: "--output-dir DIR", flags: "--output-dir DIR\tNew local directory; existing paths are preserved\n" + workFlags},
	}},
	{path: "db", summary: "Manage local engine DB references", children: []commandHelp{
		{path: "db add", summary: "Register an engine-side source reference", usage: "NAME [--url-env ENV]", flags: "--url-env ENV\tSource URL variable on the engine (default FORKDEN_SOURCE_URL)", example: "forkden db add sample --url-env FORKDEN_SOURCE_URL"},
		{path: "db list", summary: "List registered profiles"},
		{path: "db show", summary: "Show a profile", usage: "NAME"},
		{path: "db check", summary: "Verify the connection and pinned database", usage: "NAME"},
		{path: "db update", summary: "Rotate the reference for the same database", usage: "NAME --url-env ENV"},
		{path: "db remove", summary: "Remove an unused profile reference", usage: "NAME"},
	}},
	{path: "clone", summary: "Manage local engine snapshots", children: []commandHelp{
		{path: "clone create", summary: "Capture the first immutable snapshot", usage: "NAME --source PROFILE [--target-env ENV]", flags: "--source PROFILE\tRegistered source profile\n--target-env ENV\tTarget URL variable on the engine (default FORKDEN_TARGET_URL)"},
		{path: "clone list", summary: "List clones"},
		{path: "clone show", summary: "Show clone metadata and versions", usage: "NAME_OR_ID"},
		{path: "clone refresh", summary: "Publish a new immutable version", usage: "NAME_OR_ID"},
		{path: "clone remove", summary: "Remove a clone after its forks are closed", usage: "NAME_OR_ID"},
		{path: "clone prune", summary: "Recover interrupted capture and removal cleanup"},
	}},
	{path: "fork", summary: "Manage local engine experiments", children: []commandHelp{
		{path: "fork create", summary: "Create an isolated database fork", usage: "(--clone NAME_OR_ID | --source PROFILE) [--ttl 1h]", flags: "--clone NAME_OR_ID\tFork an existing clone\n--source PROFILE\tCopy a registered source instead\n--version N\tClone version; omitted or 0 means latest ready version\n--target-env ENV\tTarget URL variable, only with --source\n--ttl DURATION\tFork lifetime, 1s–24h (default 1h)", example: "forkden fork create --clone commerce --ttl 1h"},
		{path: "fork list", summary: "List forks and expiry state"},
		{path: "fork close", summary: "Close a fork and release its copy", usage: "ID"},
		{path: "fork prune", summary: "Close expired and interrupted forks"},
	}},
	{path: "query", summary: "Run one SQL statement on a local fork", usage: "--fork ID (--file FILE | --sql SQL)", flags: sqlFlags, example: "forkden query --fork fd_example --file backfill.sql"},
	{path: "check", summary: "Evaluate a read-only boolean check on a local fork", usage: "--fork ID (--file FILE | --sql SQL)", flags: sqlFlags, example: "forkden check --fork fd_example --sql 'SELECT true'"},
	{path: "diff", summary: "Review changes in a local fork", usage: "--fork ID"},
	{path: "history", summary: "Inspect local operation outcomes", usage: "--fork ID"},
	{path: "export", summary: "Write local fork SQL and a report", usage: "--fork ID --output DIR", example: "forkden export --fork fd_example --output ./review"},
	{path: "engine", summary: "Inspect the local engine", children: []commandHelp{
		{path: "engine status", summary: "Show engine and protocol readiness"},
	}},
	{path: "version", summary: "Show CLI and protocol versions"},
	{path: "completion", summary: "Generate shell completion", usage: "bash|zsh", example: "source <(forkden completion zsh)"},
}

const (
	jobFlags      = "--request-id ID\tStable key for an explicit retry of the same intent\n--wait\tFollow worker-confirmed completion\n--timeout DURATION\tPolling limit, 1s–1h (default 20m); stopping does not cancel the job"
	workFlags     = "--request-id ID\tExplicit request identity; execution is never automatically retried"
	cloudSQLFlags = "--file FILE\tRead one SQL statement from a file (up to 64 KiB)\n--sql SQL\tPass a quoted statement; mutually exclusive with --file and positional SQL\n" + workFlags
	sqlFlags      = "--fork ID\tLocal engine fork ID\n--file FILE\tRead one SQL statement from a file (up to 64 KiB)\n--sql SQL\tPass one SQL statement; mutually exclusive with --file"
)

func rootHelp() commandHelp {
	return commandHelp{summary: "Forkden — a workspace for your database experiments", children: commands}
}

func commandName(path string) string {
	parts := strings.Fields(path)
	return parts[len(parts)-1]
}

func resolveCommand(args []string) (commandHelp, int, error) {
	current := rootHelp()
	for i, word := range args {
		if len(current.children) == 0 || strings.HasPrefix(word, "-") || word == "help" {
			return current, i, nil
		}
		name := word
		if current.path == "" && word == "source" {
			name = "db"
		}
		if current.path == "cloud" {
			switch word {
			case "login", "logout", "whoami":
				for _, cmd := range commands {
					if cmd.path == word {
						cmd.path = "cloud " + word
						return cmd, i + 1, nil
					}
				}
			case "profile":
				name = "db"
			}
		}
		index := slices.IndexFunc(current.children, func(cmd commandHelp) bool { return commandName(cmd.path) == name })
		if index < 0 {
			message := fmt.Sprintf("unknown command %q", word)
			if suggestion := suggestCommand(name, current.children); suggestion != "" {
				message += "; did you mean " + suggestion + "?"
			}
			return current, i, invalidInput(message)
		}
		current = current.children[index]
	}
	return current, len(args), nil
}

func writeHelp(out io.Writer, cmd commandHelp) error {
	var text strings.Builder
	if cmd.path == "" {
		fmt.Fprintf(&text, "%s\n\nUsage: forkden COMMAND [flags]\n", cmd.summary)
	} else {
		fmt.Fprintf(&text, "%s\n\nUsage: forkden %s", cmd.summary, cmd.path)
		if len(cmd.children) > 0 {
			text.WriteString(" COMMAND")
		}
		if cmd.usage != "" {
			fmt.Fprintf(&text, " %s", cmd.usage)
		}
		text.WriteByte('\n')
	}
	if len(cmd.children) > 0 {
		if cmd.path == "" {
			for _, group := range []struct {
				title string
				names []string
			}{
				{title: "Account and workers", names: []string{"login", "whoami", "logout", "cloud"}},
				{title: "Local engine", names: []string{"db", "clone", "fork", "query", "check", "diff", "history", "export", "engine"}},
				{title: "Utilities", names: []string{"version", "completion"}},
			} {
				fmt.Fprintf(&text, "\n%s:\n", group.title)
				for _, child := range cmd.children {
					if slices.Contains(group.names, child.path) {
						fmt.Fprintf(&text, "  %s\t%s\n", child.path, child.summary)
					}
				}
			}
		} else {
			text.WriteString("\nCommands:\n")
			for _, child := range cmd.children {
				fmt.Fprintf(&text, "  %s\t%s\n", commandName(child.path), child.summary)
			}
		}
	}
	if cmd.flags != "" {
		text.WriteString("\nFlags:\n")
		for _, line := range strings.Split(cmd.flags, "\n") {
			fmt.Fprintf(&text, "  %s\n", line)
		}
	}
	text.WriteString("\nGlobal flags (before or after the command):\n  --json\tCompact JSON envelope for scripts\n  --format auto|table|json\tReadable terminal output; JSON when piped (default auto)\n  --api-url ORIGIN\tAccount API origin (FORKDEN_API_URL)\n  --config-dir DIR\tPrivate CLI settings (FORKDEN_CLI_CONFIG_DIR)\n  --socket PATH\tLocal engine socket (FORKDEN_ENGINE_SOCKET)\n  -h, --help\tShow help without connecting\n")
	if cmd.example != "" {
		fmt.Fprintf(&text, "\nExample:\n  %s\n", cmd.example)
	}
	if cmd.path == "" || cmd.path == "cloud" {
		text.WriteString("\nGet started:\n  forkden login --api-url https://app.tryforkden.com\n  forkden cloud org list\n  forkden cloud project list --org ORG_ID\n  forkden cloud project use PROJECT_ID --org ORG_ID\n")
	}
	if cmd.path == "db" {
		text.WriteString("\n'source' is an alias for 'db'. URL variables are resolved on the engine.\n")
	}
	text.WriteString("\nRun 'forkden help COMMAND' for examples and command flags.\n")
	writer := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
	if _, err := io.WriteString(writer, text.String()); err != nil {
		return err
	}
	return writer.Flush()
}

func suggestCommand(input string, choices []commandHelp) string {
	for _, candidate := range choices {
		name := commandName(candidate.path)
		if len(input) > len(name)+2 {
			continue
		}
		if len(input) >= 3 && (strings.HasPrefix(name, input) || editDistance(input, name) <= 2) {
			return "forkden " + candidate.path
		}
	}
	return ""
}

func editDistance(a, b string) int {
	left, right := []rune(a), []rune(b)
	previous := make([]int, len(right)+1)
	for i := range previous {
		previous[i] = i
	}
	for i, x := range left {
		current := make([]int, len(right)+1)
		current[0] = i + 1
		for j, y := range right {
			cost := 0
			if x != y {
				cost = 1
			}
			current[j+1] = min(current[j]+1, previous[j+1]+1, previous[j]+cost)
		}
		previous = current
	}
	return previous[len(right)]
}

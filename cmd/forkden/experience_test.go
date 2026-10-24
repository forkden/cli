package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/forkden/cli/api/cloud"
	v1 "github.com/forkden/cli/api/v1"
)

func TestEveryCommandHasOfflineContextualHelp(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "must-not-be-created")
	for _, cmd := range allCommands() {
		t.Run(cmd.path, func(t *testing.T) {
			t.Parallel()
			forms := [][]string{append(strings.Fields(cmd.path), "--help"), append([]string{"help"}, strings.Fields(cmd.path)...)}
			for _, form := range forms {
				args := append([]string{"--config-dir", dir, "--api-url", "invalid", "--socket", "/missing.sock"}, form...)
				var out, progress bytes.Buffer
				err := runWithCloud(t.Context(), args, &out, &progress, cloudApp{})
				if err != nil || !strings.Contains(out.String(), cmd.summary) || !strings.Contains(out.String(), "Usage: forkden") || progress.Len() != 0 {
					t.Errorf("runWithCloud(%v) = %q, %v; want offline contextual help and no progress", form, out.String(), err)
				}
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("help(%q) created config directory; stat = %v, want not exist", cmd.path, err)
			}
		})
	}
}

func TestCloudConventionalFlagsAndInterspersedArguments(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	for _, args := range [][]string{
		{"login", "--no-browser", "--api-url", f.server.URL},
		{"cloud", "project", "use", "--org", "org_demo", "prj_demo"},
		{"cloud", "clone", "create", "--profile", "prf_demo", "baseline", "--request-id", "req_experience_test", "--wait"},
	} {
		if _, _, err := f.run(t, args...); err != nil {
			t.Fatalf("run(%v) = %v, want successful cloud workflow", args, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.intents) != 1 || f.intents[0].Name != "baseline" || f.intents[0].IdempotencyKey != "req_experience_test" {
		t.Errorf("cloud create intents = %+v, want one baseline with the explicit request ID", f.intents)
	}
}

func TestLocalAliasesPreserveSQLAndEnvironmentReferences(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"db", "add", "--url-env", "SOURCE_REFERENCE", "sample", "--json"},
		{"query", "--sql", "--json --help --api-url untouched", "--fork", "fd_sample", "--json"},
	} {
		opts, parsed, err := parseGlobalOptions(args)
		if err != nil || !opts.compact {
			t.Fatalf("parseGlobalOptions(%v) = %+v, %v; want compact JSON", args, opts, err)
		}
		cmd, consumed, err := resolveCommand(parsed)
		if err != nil || wantsHelp(parsed[consumed:]) {
			t.Fatalf("resolveCommand(%v) = %v, want executable command", parsed, err)
		}
		client := &fakeEngine{response: v1.Response{Data: json.RawMessage(`{}`)}}
		if _, err := dispatch(t.Context(), client, orderCommandArgs(cmd, parsed, consumed), io.Discard); err != nil {
			t.Fatalf("dispatch(%v) = %v, want one engine operation", parsed, err)
		}
		if client.calls != 1 {
			t.Errorf("dispatch(%v) made %d calls, want 1", parsed, client.calls)
		}
		if args[0] == "query" && client.input.(v1.QueryInput).SQL != args[2] {
			t.Errorf("query SQL = %q, want unchanged %q", client.input.(v1.QueryInput).SQL, args[2])
		}
		if args[0] == "db" && client.input.(v1.ProfileInput).URLEnv != "SOURCE_REFERENCE" {
			t.Errorf("db input = %+v, want environment reference only", client.input)
		}
	}
}

func TestExplicitEndOfOptionsPreservesSQL(t *testing.T) {
	t.Parallel()
	input := []string{"cloud", "query", "--", "--json"}
	opts, args, err := parseGlobalOptions(input)
	cmd, consumed, resolveErr := resolveCommand(args)
	ordered := orderCommandArgs(cmd, args, consumed)
	if err != nil || resolveErr != nil || opts.compact || !slices.Equal(ordered, input) {
		t.Errorf("parseGlobalOptions(%v) = %+v, %v, %v; want literal SQL after --", input, opts, ordered, errors.Join(err, resolveErr))
	}
}

func TestEndOfOptionsCannotBeReinterpretedAsLocalFlags(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"query", "--", "--fork", "fd_sample", "--sql", "DELETE FROM users"},
		{"db", "add", "sample", "--", "--url-env", "SECRET_REFERENCE"},
	} {
		_, parsed, err := parseGlobalOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		cmd, consumed, err := resolveCommand(parsed)
		if err != nil {
			t.Fatal(err)
		}
		client := &fakeEngine{}
		_, err = dispatch(t.Context(), client, orderCommandArgs(cmd, parsed, consumed), io.Discard)
		if exitCode(err) != 2 || client.calls != 0 {
			t.Errorf("dispatch(%v) = %v, %d calls; want invalid input before engine execution", args, err, client.calls)
		}
	}
}

func TestReadableOutputAndMachineOutputStaySeparate(t *testing.T) {
	t.Parallel()
	var out, progress bytes.Buffer
	err := runWithCloud(t.Context(), []string{"version", "--format", "table"}, &out, &progress, cloudApp{})
	if err != nil || !strings.Contains(out.String(), "Version") || !strings.Contains(out.String(), version) || json.Valid(out.Bytes()) || progress.Len() != 0 {
		t.Errorf("version --format table = %q, %v; want readable version and clean stderr", out.String(), err)
	}
	for _, args := range [][]string{{"version"}, {"version", "--json"}, {"version", "--format", "json"}, {"--version", "--json"}, {"--json", "--version"}, {"-v", "--json"}} {
		out.Reset()
		progress.Reset()
		err := runWithCloud(t.Context(), args, &out, &progress, cloudApp{})
		var result struct {
			OK   bool `json:"ok"`
			Data struct {
				Version string `json:"version"`
			} `json:"data"`
		}
		if err != nil || json.Unmarshal(out.Bytes(), &result) != nil || !result.OK || result.Data.Version != version || progress.Len() != 0 {
			t.Errorf("run(%v) = %q, %v; want one JSON success envelope", args, out.String(), err)
		}
	}
}

func TestReadableErrorsHaveHintsAndPreserveFailedCheckData(t *testing.T) {
	t.Parallel()
	var out, progress bytes.Buffer
	err := runWithCloud(t.Context(), []string{"cluod", "--format", "table"}, &out, &progress, cloudApp{})
	if exitCode(err) != 2 || out.Len() != 0 || !strings.Contains(progress.String(), "did you mean forkden cloud") {
		t.Errorf("cluod = stdout %q, stderr %q, exit %d; want suggested command on stderr", out.String(), progress.String(), exitCode(err))
	}
	out.Reset()
	progress.Reset()
	output := cliOutput{out: &out, errOut: &progress, human: true, command: "check"}
	err = output.finish(json.RawMessage(`{"check_state":"failed","transaction_status":"committed","affected_rows":9007199254740993}`), &v1.Error{Code: "check_failed", Message: "SQL check returned false"})
	if exitCode(err) != 3 || !strings.Contains(out.String(), "9007199254740993") || !strings.Contains(out.String(), "committed") || !strings.Contains(progress.String(), "check_failed") {
		t.Errorf("failed check = stdout %q, stderr %q, exit %d; want exact audit data and exit 3", out.String(), progress.String(), exitCode(err))
	}
	out.Reset()
	progress.Reset()
	err = output.finish(json.RawMessage(nil), errors.New("engine is unavailable"))
	if err == nil || out.Len() != 0 || !strings.Contains(progress.String(), "engine is unavailable") {
		t.Errorf("missing operation data = stdout %q, stderr %q, %v; want error only", out.String(), progress.String(), err)
	}
}

func TestTablesKeepIdentityAndSuppressTerminalControls(t *testing.T) {
	t.Parallel()
	items := make([]cloud.Organization, 24)
	for i := range items {
		items[i] = cloud.Organization{ID: "org_example_full_identity", Name: "team\x1b[2J\r\nspoof\tvalue", Role: "owner"}
	}
	var out bytes.Buffer
	if err := renderValue(&out, items); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\r\t") || !strings.Contains(out.String(), "org_example_full_identity") || !strings.Contains(out.String(), "all 24") {
		t.Errorf("renderValue(24 organizations) = %q; want bounded safe table with intact IDs", out.String())
	}
	out.Reset()
	if err := renderValue(&out, []cloud.Organization{}); err != nil || !strings.Contains(out.String(), "No results") {
		t.Errorf("renderValue(empty) = %q, %v; want empty-state text", out.String(), err)
	}
}

func TestInputFailuresRemainJSONAndExitTwo(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"version", "extra", "--json"}, {"help", "cloud", "unknown", "--json"},
		{"--format", "wrong", "--json", "version"}, {"--json", "--format", "table", "version"},
		{"completion", "powershell", "--json"}, {"cloud", "fork", "craete", "--json"},
	} {
		var out, progress bytes.Buffer
		err := runWithCloud(t.Context(), args, &out, &progress, cloudApp{})
		var result struct {
			OK    bool `json:"ok"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if exitCode(err) != 2 || json.Unmarshal(out.Bytes(), &result) != nil || result.OK || result.Error.Code != "invalid_input" || progress.Len() != 0 {
			t.Errorf("run(%v) = %q, %v; want one JSON invalid_input envelope and exit 2", args, out.String(), err)
		}
	}
}

func TestCancellationAndCloudFalseCheckExitCodes(t *testing.T) {
	t.Parallel()
	if got := exitCode(context.Canceled); got != 130 {
		t.Errorf("exitCode(context.Canceled) = %d, want 130", got)
	}
	if got := exitCode(&cloud.Error{Code: "check_failed"}); got != 3 {
		t.Errorf("exitCode(cloud check_failed) = %d, want 3", got)
	}
}

func TestCompletionScriptsParseWithoutAccountAccess(t *testing.T) {
	t.Parallel()
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is unavailable", shell)
			}
			script, err := completionScript(shell)
			if err != nil {
				t.Fatal(err)
			}
			//nolint:gosec // The shell is from a fixed allowlist; only generated completion code is parsed.
			command := exec.CommandContext(t.Context(), path, "-n")
			command.Stdin = strings.NewReader(script)
			if output, err := command.CombinedOutput(); err != nil {
				t.Errorf("%s -n completion script = %s, %v; want valid syntax", shell, output, err)
			}
		})
	}
}

func TestBashCompletionSuggestsOnlyCurrentCommandContext(t *testing.T) {
	t.Parallel()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	script, err := completionScript("bash")
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // Executes generated completion and a fixed test invocation without user input.
	command := exec.CommandContext(t.Context(), shell, "-c", script+"\nCOMP_WORDS=(forkden --api-url https://example.com cloud fork cr); COMP_CWORD=5; _forkden; printf '%s\\n' \"${COMPREPLY[@]}\"")
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "create" {
		t.Errorf("bash complete cloud fork cr = %q, %v; want create", output, err)
	}
}

func TestZshCompletionKeepsShellEnvironment(t *testing.T) {
	t.Parallel()
	shell, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is unavailable")
	}
	script, err := completionScript("zsh")
	if err != nil {
		t.Fatal(err)
	}
	setup := "compdef() { :; }; compadd() { printf '%s\\n' \"$@\"; }; saved_path=$PATH\n"
	check := "\nwords=(forkden --api-url https://example.com cloud fork cr); CURRENT=6; _forkden; [[ $saved_path == $PATH ]]"
	//nolint:gosec // Executes generated completion and fixed shell-environment checks without user input.
	command := exec.CommandContext(t.Context(), shell, "-c", setup+script+check)
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "create\n") || strings.Contains(string(output), "org\n") {
		t.Errorf("zsh complete cloud fork cr = %q, %v; want fork candidates and intact PATH", output, err)
	}
}

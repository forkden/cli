package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/forkden/cli/api/v1"
)

type engine interface {
	Execute(context.Context, v1.Action, any) (v1.Response, error)
	Status(context.Context) (v1.Response, error)
}

func dispatch(ctx context.Context, client engine, args []string, out io.Writer) (any, error) {
	switch args[0] {
	case "db", "source":
		return profileCommand(ctx, client, args[1:], out)
	case "fork":
		return forkCommand(ctx, client, args[1:], out)
	case "query", "check", "history", "diff", "export":
		return workspaceCommand(ctx, client, args, out)
	case "engine":
		if len(args) != 2 || args[1] != "status" {
			return nil, invalidInput("use engine status")
		}
		response, err := client.Status(ctx)
		return response.Data, err
	default:
		return nil, invalidInput("unknown command; run forkden help")
	}
}

func execute(ctx context.Context, client engine, action v1.Action, input any) (any, error) {
	response, err := client.Execute(ctx, action, input)
	return response.Data, err
}

func profileCommand(ctx context.Context, client engine, args []string, out io.Writer) (any, error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || (len(args) == 2 && args[1] == "--help") {
		_, err := fmt.Fprint(out, profileUsage)
		return nil, errors.Join(flag.ErrHelp, err)
	}
	if len(args) == 1 && args[0] == "list" {
		return execute(ctx, client, v1.ProfileList, struct{}{})
	}
	if len(args) < 2 {
		return nil, invalidInput("db command requires a profile name; run forkden db help")
	}
	switch args[0] {
	case "add", "update":
		flags := commandFlags("db " + args[0])
		defaultEnv := ""
		if args[0] == "add" {
			defaultEnv = "FORKDEN_SOURCE_URL"
		}
		env := flags.String("url_env", defaultEnv, "engine environment variable containing source URL")
		if err := parse(flags, args[2:], out); err != nil {
			return nil, err
		}
		if *env == "" {
			return nil, invalidInput("--url_env is required")
		}
		action := v1.ProfileAdd
		if args[0] == "update" {
			action = v1.ProfileUpdate
		}
		return execute(ctx, client, action, v1.ProfileInput{Name: args[1], URLEnv: *env})
	case "show", "check", "remove":
		if len(args) != 2 {
			return nil, invalidInput("db command requires exactly one profile name")
		}
		action := map[string]v1.Action{"show": v1.ProfileShow, "check": v1.ProfileCheck, "remove": v1.ProfileRemove}[args[0]]
		return execute(ctx, client, action, v1.ProfileName{Name: args[1]})
	default:
		return nil, invalidInput("unknown db command; run forkden db help")
	}
}

func forkCommand(ctx context.Context, client engine, args []string, out io.Writer) (any, error) {
	if len(args) == 0 {
		return nil, invalidInput("use fork create, list, close or prune")
	}
	switch args[0] {
	case "list", "prune":
		if len(args) != 1 {
			return nil, invalidInput("unexpected fork arguments")
		}
		action := v1.ForkList
		if args[0] == "prune" {
			action = v1.ForkPrune
		}
		return execute(ctx, client, action, struct{}{})
	case "close":
		if len(args) != 2 {
			return nil, invalidInput("use fork close ID")
		}
		return execute(ctx, client, v1.ForkClose, v1.ForkID{ID: args[1]})
	case "create":
		flags := commandFlags("fork create")
		source := flags.String("source", "", "registered source profile")
		target := flags.String("target_env", "FORKDEN_TARGET_URL", "dedicated target administrator URL env on engine")
		ttl := flags.Duration("ttl", time.Hour, "fork lifetime, whole seconds from 1s to 24h")
		if err := parse(flags, args[1:], out); err != nil {
			return nil, err
		}
		if *source == "" || *ttl < time.Second || *ttl > 24*time.Hour || *ttl%time.Second != 0 {
			return nil, invalidInput("--source and a whole-second ttl between 1s and 24h are required")
		}
		return execute(ctx, client, v1.ForkCreate, v1.CreateInput{Source: *source, TargetEnv: *target, TTLSeconds: int64(*ttl / time.Second)})
	default:
		return nil, invalidInput("unknown fork command")
	}
}

func workspaceCommand(ctx context.Context, client engine, args []string, out io.Writer) (any, error) {
	command := args[0]
	flags := commandFlags(command)
	id := flags.String("fork", "", "engine-owned fork ID")
	var sql, file, output *string
	if command == "query" || command == "check" {
		sql = flags.String("sql", "", "one SQL statement")
		file = flags.String("file", "", "file containing one SQL statement")
	}
	if command == "export" {
		output = flags.String("output", "", "new artifact directory on this machine")
	}
	if err := parse(flags, args[1:], out); err != nil {
		return nil, err
	}
	if *id == "" {
		return nil, invalidInput("--fork is required")
	}
	switch command {
	case "query", "check":
		statement, err := statementInput(*sql, *file)
		if err != nil {
			return nil, err
		}
		action := v1.Query
		if command == "check" {
			action = v1.Check
		}
		return execute(ctx, client, action, v1.QueryInput{ForkID: *id, SQL: statement})
	case "history", "diff":
		action := v1.History
		if command == "diff" {
			action = v1.Review
		}
		return execute(ctx, client, action, v1.ForkID{ID: *id})
	case "export":
		if *output == "" {
			return nil, invalidInput("--output is required")
		}
		response, err := client.Execute(ctx, v1.Export, v1.ForkID{ID: *id})
		if err != nil {
			return response.Data, err
		}
		var artifact v1.ExportData
		if err := json.Unmarshal(response.Data, &artifact); err != nil || artifact.SQL == "" || !json.Valid(artifact.Report) || bytes.Equal(artifact.Report, []byte("null")) {
			return nil, errors.New("engine returned invalid export artifacts")
		}
		return writeExport(*output, artifact)
	default:
		return nil, invalidInput("unknown workspace command")
	}
}

func commandFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	return flags
}

func parse(flags *flag.FlagSet, args []string, out io.Writer) error {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(out)
			flags.PrintDefaults()
			return err
		}
		return invalidInput(err.Error())
	}
	if flags.NArg() != 0 {
		return invalidInput("unexpected arguments")
	}
	return nil
}

func statementInput(sql, file string) (string, error) {
	if (sql == "") == (file == "") {
		return "", invalidInput("provide exactly one of --sql or --file")
	}
	if file != "" {
		//nolint:gosec // Reading the explicitly selected local SQL file is the CLI operation.
		input, err := os.Open(file)
		if err != nil {
			return "", err
		}
		data, readErr := io.ReadAll(io.LimitReader(input, (64<<10)+1))
		if err := errors.Join(readErr, input.Close()); err != nil {
			return "", err
		}
		sql = string(data)
	}
	if len(sql) == 0 || len(sql) > 64<<10 {
		return "", invalidInput("SQL must contain between 1 and 65536 bytes")
	}
	return sql, nil
}

func writeExport(dir string, artifact v1.ExportData) (_ map[string]any, retErr error) {
	var report bytes.Buffer
	if err := json.Indent(&report, artifact.Report, "", "  "); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create new export directory (existing paths are preserved): %w", err)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(dir))
		}
	}()
	sqlPath, reportPath := filepath.Join(dir, "changes.sql"), filepath.Join(dir, "report.json")
	if err := os.WriteFile(sqlPath, []byte(artifact.SQL), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(reportPath, report.Bytes(), 0o600); err != nil {
		return nil, err
	}
	return map[string]any{"sql_path": sqlPath, "report_path": reportPath}, nil
}

const profileUsage = `DB profiles use environment references on the engine machine.

  db add NAME --url_env ENV
  db list
  db show NAME
  db check NAME
  db update NAME --url_env ENV
  db remove NAME

source is an alias for db. Removing a profile does not delete a database or secret.
`

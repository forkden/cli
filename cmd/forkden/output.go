package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/forkden/cli/api/cloud"
	v1 "github.com/forkden/cli/api/v1"
)

type cliOutput struct {
	out     io.Writer
	errOut  io.Writer
	human   bool
	compact bool
	command string
}

func newOutput(out, progress io.Writer, opts globalOptions) cliOutput {
	human := opts.format == "table" || (opts.format == "auto" && isTerminal(out))
	return cliOutput{out: out, errOut: progress, human: human && !opts.compact, compact: opts.compact}
}

func (o cliOutput) finish(value any, operationErr error) error {
	if !o.human {
		return errors.Join(operationErr, writeOutput(o.out, value, operationErr, o.compact))
	}
	var rendered bytes.Buffer
	if value != nil {
		if err := renderValue(&rendered, value); err != nil {
			return errors.Join(operationErr, err)
		}
	} else if operationErr == nil {
		rendered.WriteString("Done.\n")
	}
	if rendered.Len() > 0 {
		if _, err := io.Copy(o.out, &rendered); err != nil {
			return errors.Join(operationErr, err)
		}
	}
	if operationErr != nil {
		publicErr := publicError(operationErr)
		message := fmt.Sprintf("Error [%s]: %s\n", terminalText(publicErr.Code), terminalText(publicErr.Message))
		if hint := errorHint(publicErr.Code, o.command); hint != "" {
			message += "\n" + hint + "\n"
		}
		_, err := io.WriteString(o.errOut, message)
		return errors.Join(operationErr, err)
	}
	if hint := nextCommand(o.command, value); hint != "" {
		_, err := fmt.Fprintln(o.out, "\nNext:", hint)
		return err
	}
	return nil
}

func isTerminal(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0 && file.Name() != os.DevNull
}

func publicError(err error) *v1.Error {
	var engineErr *v1.Error
	if errors.As(err, &engineErr) {
		return engineErr
	}
	var cloudErr *cloud.Error
	if errors.As(err, &cloudErr) {
		return &v1.Error{Code: cloudErr.Code, Message: cloudErr.Message}
	}
	if errors.Is(err, context.Canceled) {
		return &v1.Error{Code: "cancelled", Message: "Command interrupted."}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &v1.Error{Code: "timeout", Message: "Command timed out."}
	}
	return &v1.Error{Code: "client_error", Message: err.Error()}
}

func renderValue(out io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode command output: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("decode command output: %w", err)
	}
	if decoded == nil {
		return nil
	}
	return renderNode(out, decoded, "")
}

func renderNode(out io.Writer, node any, indent string) error {
	switch value := node.(type) {
	case map[string]any:
		writer := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
		for _, key := range orderedKeys(value) {
			if !isScalar(value[key]) {
				continue
			}
			if _, err := fmt.Fprintf(writer, "%s%s\t%s\n", indent, fieldLabel(key), scalarText(value[key])); err != nil {
				return err
			}
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		for _, key := range orderedKeys(value) {
			if isScalar(value[key]) {
				continue
			}
			if _, err := fmt.Fprintf(out, "\n%s%s\n", indent, fieldLabel(key)); err != nil {
				return err
			}
			if err := renderNode(out, value[key], indent+"  "); err != nil {
				return err
			}
		}
		return nil
	case []any:
		return renderList(out, value, indent)
	default:
		_, err := fmt.Fprintln(out, indent+scalarText(value))
		return err
	}
}

func renderList(out io.Writer, items []any, indent string) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(out, indent+"No results.")
		return err
	}
	first, isObject := items[0].(map[string]any)
	keys := listColumns(first)
	if !isObject || len(keys) == 0 {
		for _, item := range items {
			if err := renderNode(out, item, indent); err != nil {
				return err
			}
		}
		return nil
	}
	writer := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
	for _, key := range keys {
		if _, err := fmt.Fprint(writer, indent+strings.ToUpper(fieldLabel(key))+"\t"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(writer); err != nil {
		return err
	}
	for _, item := range items[:min(len(items), 20)] {
		row, ok := item.(map[string]any)
		if !ok {
			return errors.New("cannot render mixed result types; use --json")
		}
		for _, key := range keys {
			if _, err := fmt.Fprint(writer, indent+scalarText(row[key])+"\t"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	noun := "results"
	if len(items) == 1 {
		noun = "result"
	}
	_, err := fmt.Fprintf(out, "\n%s%d %s. Use --json for every field", indent, len(items), noun)
	if err != nil {
		return err
	}
	if len(items) > 20 {
		_, err = fmt.Fprintf(out, "\n%sShowing the first 20; --json includes all %d results", indent, len(items))
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, ".")
	return err
}

func listColumns(row map[string]any) []string {
	keys := make([]string, 0, 6)
	for _, key := range []string{"id", "name", "kind", "status", "role", "database_type", "version", "number", "latest_version", "clone_version", "check_state", "cleanup_state", "expires_at"} {
		if value, ok := row[key]; ok && isScalar(value) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		for _, key := range orderedKeys(row) {
			if isScalar(row[key]) {
				keys = append(keys, key)
			}
		}
	}
	return keys[:min(len(keys), 6)]
}

func orderedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	priority := []string{"id", "name", "kind", "status", "version", "api_version", "request_id"}
	slices.SortStableFunc(keys, func(a, b string) int {
		ai, bi := slices.Index(priority, a), slices.Index(priority, b)
		if ai < 0 {
			ai = len(priority)
		}
		if bi < 0 {
			bi = len(priority)
		}
		return ai - bi
	})
	return keys
}

func isScalar(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

func scalarText(value any) string {
	if value == nil {
		return "—"
	}
	if text, ok := value.(string); ok {
		if text == "" {
			return "—"
		}
		if date, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return date.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		return terminalText(text)
	}
	return terminalText(fmt.Sprint(value))
}

func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, text)
}

func fieldLabel(key string) string {
	words := strings.Split(key, "_")
	for i, word := range words {
		switch word {
		case "id", "sql", "api", "url", "os", "ttl":
			words[i] = strings.ToUpper(word)
		default:
			if i == 0 && word != "" {
				letters := []rune(word)
				letters[0] = unicode.ToUpper(letters[0])
				words[i] = string(letters)
			}
		}
	}
	return terminalText(strings.Join(words, " "))
}

func errorHint(code, command string) string {
	switch code {
	case "unauthorized":
		return "Sign in with 'forkden login --api-url ORIGIN'."
	case "engine_unavailable", "connection_failed":
		return "Start the local engine, then run 'forkden engine status --socket PATH'."
	case "transport_error", "cancelled", "timeout", "operation_failed", "job_failed", "unknown_outcome", "sql_failed":
		if slices.Contains([]string{"cloud query", "cloud check", "cloud schema", "cloud history", "cloud review", "cloud export"}, command) {
			return "Inspect 'forkden cloud history' and 'forkden cloud review' before retrying SQL."
		}
		if strings.HasPrefix(command, "cloud") {
			return "Inspect 'forkden cloud job list' before retrying. Stopping a command does not cancel a worker job."
		}
		return "Inspect 'forkden fork list' and fork history before retrying a change."
	case "engine_busy":
		return "Wait for the current engine operation to finish, then try again."
	case "check_failed":
		return "The check returned false. Review its operation data and SQL before continuing."
	case "invalid_input":
		if command == "" {
			return "Run 'forkden help' for commands and examples."
		}
		return "Run 'forkden help " + command + "' for usage and examples."
	default:
		return ""
	}
}

func nextCommand(command string, value any) string {
	switch command {
	case "login", "cloud login":
		return "forkden cloud org list"
	case "cloud org list":
		return "forkden cloud project list --org ORG_ID"
	case "cloud project list":
		return "forkden cloud project use PROJECT_ID --org ORG_ID"
	case "cloud project use":
		return "forkden cloud db list"
	case "cloud session create":
		return "forkden cloud schema"
	}
	if result, ok := value.(map[string]any); ok {
		if job, ok := result["job"].(cloud.Job); ok && !job.Terminal() {
			return "forkden cloud job wait " + terminalText(job.ID)
		}
	}
	return ""
}

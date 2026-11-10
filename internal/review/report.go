package review

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

var jsonUnmarshal = json.Unmarshal

// artifactFiles are the generation files a manifest covers and export copies.
var artifactFiles = []string{"report.json", "report.md", "changes.sql", "checks.json", "manifest.json"}

// Report is report.json, schema 1.
type Report struct {
	Schema           int            `json:"schema"`
	Name             string         `json:"name"`
	Status           string         `json:"status"` // ok or partial
	GeneratedAt      string         `json:"generated_at"`
	Mode             string         `json:"mode"` // fork or dsn
	Base             Endpoint       `json:"base"`
	Work             Endpoint       `json:"work"`
	BaseChanged      bool           `json:"base_changed"`
	BaseChangedItems []string       `json:"base_changed_items,omitempty"`
	Totals           Totals         `json:"totals"`
	Tables           []TableReport  `json:"tables"`
	Unsupported      []Unsupported  `json:"unsupported"`
	SchemaChanges    []SchemaChange `json:"schema_changes"`
	ChangesTruncated bool           `json:"changes_truncated"`
	Timings          Timings        `json:"timings"`
}

// Endpoint is one side of a comparison: where it was read and what it contained.
type Endpoint struct {
	DSN             string `json:"dsn_redacted"`
	Fingerprint     string `json:"fingerprint"`
	ForkFingerprint string `json:"fork_fingerprint,omitempty"`
}

// Totals sums the row changes across compared tables.
type Totals struct {
	TablesChanged int   `json:"tables_changed"`
	Inserted      int64 `json:"inserted"`
	Updated       int64 `json:"updated"`
	Deleted       int64 `json:"deleted"`
}

// TableReport is one compared table with its counts and up to three sample rows.
type TableReport struct {
	Schema   string   `json:"schema"`
	Name     string   `json:"name"`
	Status   string   `json:"status"` // changed or unchanged
	Inserted int64    `json:"inserted"`
	Updated  int64    `json:"updated"`
	Deleted  int64    `json:"deleted"`
	Samples  []Sample `json:"samples"`
}

// Sample is one changed row: its key and its row_to_json values before and after.
type Sample struct {
	Kind string          `json:"kind"` // insert, update or delete
	PK   json.RawMessage `json:"pk"`
	Old  json.RawMessage `json:"old,omitempty"`
	New  json.RawMessage `json:"new,omitempty"`
}

// Unsupported is a table that was not compared, with the reason.
type Unsupported struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Reason string `json:"reason"`
}

// SchemaChange is a difference in an object that is not a compared table.
type SchemaChange struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Change string `json:"change"` // added, removed or changed
}

// Timings records how long fork and diff took, from the artifacts rather than memory.
type Timings struct {
	DumpMS        int64 `json:"dump_ms"`
	RestoreMS     int64 `json:"restore_ms"`
	DiffMS        int64 `json:"diff_ms"`
	TablesScanned int   `json:"tables_scanned"`
}

// Checks is checks.json, schema 1.
type Checks struct {
	Schema          int           `json:"schema"`
	Name            string        `json:"name"`
	Target          string        `json:"target"`
	WorkFingerprint string        `json:"work_fingerprint"`
	GeneratedAt     string        `json:"generated_at"`
	Results         []CheckResult `json:"checks"`
}

// CheckResult is one check statement's outcome.
type CheckResult struct {
	Name   string `json:"name"`
	Result string `json:"result"` // pass, fail or error
	Detail string `json:"detail,omitempty"`
}

// generation is manifest.json: the hash of every file one writer produced, written last.
type generation struct {
	Schema      int               `json:"schema"`
	GeneratedAt string            `json:"generated_at"`
	Files       map[string]string `json:"files"`
}

// newStage creates the private directory a writer fills before commitStage.
func newStage(dir string) (string, error) {
	stage := filepath.Join(dir, ".tmp-"+strconv.Itoa(os.Getpid()))
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	return stage, os.Mkdir(stage, 0o700)
}

// commitStage renames every staged file into dir, removes the names in drop and
// writes manifest.json last with the hash of each file (staged and kept). A reader
// that finds hashes not matching the files knows a writer stopped part-way.
func commitStage(dir, stage, generatedAt string, keep, drop []string) error {
	defer func() { _ = os.RemoveAll(stage) }()
	hashes := map[string]string{}
	for _, name := range keep {
		sum, err := fileHash(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		hashes[name] = sum
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sum, err := fileHash(filepath.Join(stage, entry.Name()))
		if err != nil {
			return err
		}
		hashes[entry.Name()] = sum
	}
	// The old manifest goes first so no reader accepts a half-replaced generation.
	if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, name := range drop {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(stage, entry.Name()), filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	manifest, err := json.MarshalIndent(generation{Schema: 1, GeneratedAt: generatedAt, Files: hashes}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, "manifest.json", append(manifest, '\n'))
}

func fileHash(path string) (string, error) {
	//nolint:gosec // path is inside the review's private directory.
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, file)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readGeneration accepts only a complete generation: every listed file present with its hash.
func readGeneration(dir string) (*generation, error) {
	//nolint:gosec // dir is the review's private directory.
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var gen generation
	if err := json.Unmarshal(data, &gen); err != nil || gen.Schema != 1 {
		return nil, errIncomplete
	}
	for name, want := range gen.Files {
		if !slices.Contains(artifactFiles, name) {
			return nil, errIncomplete
		}
		got, err := fileHash(filepath.Join(dir, name))
		if err != nil || got != want {
			return nil, errIncomplete
		}
	}
	return &gen, nil
}

var errIncomplete = errors.New("incomplete generation")

func (r *runner) incomplete() error {
	return newError("invalid_input", "incomplete generation in "+r.dir+"; a previous writer stopped part-way or a file was edited",
		"rerun forkden review diff "+r.cmd.Name, "nothing was changed", "")
}

func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

func readJSON(path string, value any) error {
	//nolint:gosec // path is inside the review's private directory.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// renderMarkdown is the approver-readable view of report.json and checks.json.
func renderMarkdown(report *Report, checks *Checks) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Review: %s\n\n", report.Name)
	status := "**ok** — every table in scope was compared"
	if report.Status != "ok" {
		status = "**partial** — some tables could not be compared; see *Not compared* below"
	}
	fmt.Fprintf(&b, "Status: %s  \nGenerated: %s  \n", status, report.GeneratedAt)
	fmt.Fprintf(&b, "Before (base): `%s`  \nAfter (work): `%s`\n\n", report.Base.DSN, report.Work.DSN)
	fmt.Fprintf(&b, "| Tables changed | Inserted | Updated | Deleted |\n|---:|---:|---:|---:|\n| %d | %d | %d | %d |\n\n",
		report.Totals.TablesChanged, report.Totals.Inserted, report.Totals.Updated, report.Totals.Deleted)
	if report.BaseChanged {
		fmt.Fprintf(&b, "> **The base copy changed after fork** (%s). The comparison is against the base as it is now, not as it was captured.\n\n",
			strings.Join(report.BaseChangedItems, ", "))
	}
	b.WriteString("## Changes\n\n")
	changed := 0
	for _, t := range report.Tables {
		if t.Status != "changed" {
			continue
		}
		changed++
		fmt.Fprintf(&b, "### %s.%s\n\n%d inserted, %d updated, %d deleted.\n\n", t.Schema, t.Name, t.Inserted, t.Updated, t.Deleted)
		for _, s := range t.Samples {
			writeSample(&b, s)
		}
		if shown := len(t.Samples); int64(shown) < t.Inserted+t.Updated+t.Deleted {
			fmt.Fprintf(&b, "_%d of %d changed rows shown; changes.sql lists every row._\n\n", shown, t.Inserted+t.Updated+t.Deleted)
		}
	}
	if changed == 0 {
		b.WriteString("No row changed in the compared tables.\n\n")
	}
	if unchanged := len(report.Tables) - changed; unchanged > 0 {
		fmt.Fprintf(&b, "%d compared tables have no changes.\n\n", unchanged)
	}
	if len(report.Unsupported) > 0 {
		b.WriteString("## Not compared\n\n| Table | Reason |\n|---|---|\n")
		for _, u := range report.Unsupported {
			fmt.Fprintf(&b, "| %s.%s | %s |\n", u.Schema, u.Table, reasonText(u.Reason))
		}
		b.WriteString("\n")
	}
	if len(report.SchemaChanges) > 0 {
		b.WriteString("## Schema changes outside tables\n\n")
		for _, c := range report.SchemaChanges {
			fmt.Fprintf(&b, "- %s %s: %s\n", c.Change, c.Kind, c.Name)
		}
		b.WriteString("\n")
	}
	if checks != nil {
		b.WriteString("## Checks\n\n| Check | Result |\n|---|---|\n")
		for _, c := range checks.Results {
			result := c.Result
			if c.Detail != "" {
				result += " — " + strings.ReplaceAll(c.Detail, "\n", " ")
			}
			fmt.Fprintf(&b, "| %s | %s |\n", escapeCell(c.Name), escapeCell(result))
		}
		if checks.WorkFingerprint == report.Work.Fingerprint {
			fmt.Fprintf(&b, "\nChecks ran %s on the same work state this report compares.\n\n", checks.GeneratedAt)
		} else {
			fmt.Fprintf(&b, "\n**Checks ran %s on a different work state than this report compares** (the work copy changed in between). Rerun diff and check together.\n\n", checks.GeneratedAt)
		}
	}
	b.WriteString("## Fingerprints\n\n")
	fmt.Fprintf(&b, "- base: `%s`\n", report.Base.Fingerprint)
	if report.Base.ForkFingerprint != "" {
		fmt.Fprintf(&b, "- base at fork: `%s`\n", report.Base.ForkFingerprint)
	}
	fmt.Fprintf(&b, "- work: `%s`\n\n", report.Work.Fingerprint)
	t := report.Timings
	fmt.Fprintf(&b, "Timings: dump %d ms, restore %d ms, diff %d ms, %d tables scanned.\n\n", t.DumpMS, t.RestoreMS, t.DiffMS, t.TablesScanned)
	b.WriteString("_This report was produced on the developer's machine by `forkden review`. It contains row values from the copied database; keep it out of shared folders._\n")
	return []byte(b.String())
}

func writeSample(b *strings.Builder, s Sample) {
	fmt.Fprintf(b, "- **%s** `%s`\n", s.Kind, compactJSON(s.PK))
	switch s.Kind {
	case "insert":
		fmt.Fprintf(b, "  - new: `%s`\n", compactJSON(s.New))
	case "delete":
		fmt.Fprintf(b, "  - old: `%s`\n", compactJSON(s.Old))
	default:
		var before, after map[string]json.RawMessage
		_ = json.Unmarshal(s.Old, &before)
		_ = json.Unmarshal(s.New, &after)
		keys := make([]string, 0, len(after))
		for key := range after {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if string(before[key]) != string(after[key]) {
				fmt.Fprintf(b, "  - %s: `%s` → `%s`\n", key, before[key], after[key])
			}
		}
	}
	b.WriteString("\n")
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return strings.ReplaceAll(buf.String(), "`", "'")
}

func escapeCell(text string) string { return strings.ReplaceAll(text, "|", `\|`) }

func reasonText(reason string) string {
	switch reason {
	case "no_primary_key":
		return "no primary key: rows cannot be matched"
	case "pk_type":
		return "primary key type is not comparable"
	case "schema_changed":
		return "table definition differs between base and work"
	case "too_large":
		return fmt.Sprintf("over the %s / %d-row cap", bytesText(maxTableBytes), int64(maxTableRows))
	}
	return reason
}

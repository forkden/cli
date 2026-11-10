package review

import (
	"bytes"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	v1 "github.com/forkden/cli/api/v1"
)

func TestNameMustBeASafeIdentifierPrefix(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "Demo", "1demo", "demo-x", "demo;drop", strings.Repeat("a", 42), "_demo"} {
		_, err := Run(t.Context(), Command{Action: "diff", Name: name, Root: t.TempDir()}, nil)
		if code(err) != "invalid_input" {
			t.Errorf("name %q: %v; want invalid_input before any connection", name, err)
		}
	}
	for _, name := range []string{"demo", "d", "demo_2", strings.Repeat("a", 41)} {
		if !namePattern.MatchString(name) {
			t.Errorf("name %q should be accepted", name)
		}
	}
}

func TestRedactAndScrubNeverKeepPasswords(t *testing.T) {
	t.Parallel()
	//nolint:gosec // Fake credentials that redaction must remove.
	cases := map[string]string{
		"postgres://app:s3cret@db:5432/shop?sslmode=require": "postgres://app@db:5432/shop?sslmode=require",
		"postgresql://app@db/shop":                           "postgresql://app@db/shop",
		"host=db user=app password=s3cret dbname=shop":       "host=db user=app  dbname=shop",
		"host=db password='a b\\'c' dbname=shop":             "host=db  dbname=shop",
		"":                                                   "",
	}
	for in, want := range cases {
		if got := redactDSN(in); got != want {
			t.Errorf("redactDSN(%q) = %q; want %q", in, got, want)
		}
	}
	for _, text := range []string{
		`pg_dump: error: connection to "postgres://app:s3cret@db/shop" failed`,
		`could not connect: host=db password=s3cret`,
	} {
		if got := scrub(text); strings.Contains(got, "s3cret") {
			t.Errorf("scrub(%q) = %q; password kept", text, got)
		}
	}
}

func TestWithDatabaseKeepsEveryOtherOption(t *testing.T) {
	t.Parallel()
	if got := withDatabase("postgres://app@db:5432/postgres?sslmode=require", "demo_base"); got != "postgres://app@db:5432/demo_base?sslmode=require" {
		t.Errorf("url form = %q", got)
	}
	if got := withDatabase("host=db user=app dbname=postgres", "demo_work"); got != "host=db user=app dbname=postgres dbname='demo_work'" {
		t.Errorf("keyword form = %q", got)
	}
	cfg, err := parseDSN(withDatabase("host=db user=app dbname=postgres", "demo_work"))
	if err != nil || cfg.Database != "demo_work" {
		t.Errorf("later dbname must win: %v, %v", cfg, err)
	}
	if got := withDatabase("", "demo_base"); got != "dbname='demo_base'" {
		t.Errorf("environment form = %q", got)
	}
}

func TestCheckGrammar(t *testing.T) {
	t.Parallel()
	statements, err := parseChecks("-- name: no nulls\nSELECT count(*) = 0\n  FROM t;\n\n\nSELECT true\n\n-- only a comment\n\r\n-- name: last\nSELECT false;")
	if err != nil {
		t.Fatal(err)
	}
	want := []checkStatement{{"no nulls", "SELECT count(*) = 0\n  FROM t"}, {"2", "SELECT true"}, {"last", "SELECT false"}}
	if !slices.Equal(statements, want) {
		t.Fatalf("parseChecks = %#v; want %#v", statements, want)
	}
	for _, bad := range []string{"", "\n\n-- just a comment\n", "-- name: empty\n\nSELECT true"} {
		if _, err := parseChecks(bad); err == nil {
			t.Errorf("parseChecks(%q) should fail", bad)
		}
	}
	if !joinedStatements.MatchString("SELECT 1;\nSELECT true") || joinedStatements.MatchString("SELECT true;") {
		t.Error("joined-statement hint detection is wrong")
	}
}

func TestCheckFileUsesTheSharedSizeCap(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "checks.sql")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), v1.MaxStatementBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheckFile(path); err == nil {
		t.Fatal("a check file over 64 KiB must be refused")
	}
}

func TestCompareValuesMatchesServerOrdering(t *testing.T) {
	t.Parallel()
	num := func(s string) pgtype.Numeric {
		var n pgtype.Numeric
		if err := n.Scan(s); err != nil {
			t.Fatal(err)
		}
		return n
	}
	now := time.Now()
	ordered := [][]any{
		{int16(-3), int32(2), int64(1 << 40)},
		{num("-Infinity"), num("-1.5"), num("0.25"), num("12"), num("Infinity"), num("NaN")},
		{"B", "a", "a\u00e9", "b"}, // bytewise, as COLLATE "C"
		{[]byte{0}, []byte{0, 1}, []byte{1}},
		{[16]byte{0}, [16]byte{1}},
		{false, true},
		{pgtype.NegativeInfinity, now.Add(-time.Hour), now, pgtype.Infinity},
	}
	for _, values := range ordered {
		for i := 1; i < len(values); i++ {
			if c, err := compareValues(values[i-1], values[i]); err != nil || c >= 0 {
				t.Errorf("compare(%v, %v) = %d, %v; want < 0", values[i-1], values[i], c, err)
			}
			if c, err := compareValues(values[i], values[i]); err != nil || c != 0 {
				t.Errorf("compare(%v, itself) = %d, %v", values[i], c, err)
			}
		}
	}
	if _, err := compareValues("1", int64(1)); err == nil {
		t.Error("mismatched key types must not compare silently")
	}
	if c, _ := compareKeys([]any{int64(1), "b"}, []any{int64(1), "a"}); c <= 0 {
		t.Error("composite keys compare column by column")
	}
	if numericRat(num("1.50")).Cmp(big.NewRat(3, 2)) != 0 {
		t.Error("numeric scale handling is wrong")
	}
}

func TestLiteralQuotingAndPatterns(t *testing.T) {
	t.Parallel()
	values := []any{"it's", nil, `C:\path`}
	if got := literal(values, 0, "text"); got != "'it''s'::text" {
		t.Errorf("literal = %s", got)
	}
	if got := literal(values, 1, "integer"); got != "NULL" {
		t.Errorf("NULL literal = %s", got)
	}
	if got := literal(values, 2, "text"); got != `'C:\path'::text` {
		t.Errorf("backslashes stay literal under standard_conforming_strings: %s", got)
	}
	if got := dumpPattern(`My"Schema`, "t*?"); got != `"My""Schema"."t*?"` {
		t.Errorf("dumpPattern = %s", got)
	}
}

func TestReportIsWrittenAsOneGeneration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stage, err := newStage(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.json", "report.md"} {
		if err := os.WriteFile(filepath.Join(stage, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := commitStage(dir, stage, "2026-11-10T00:00:00Z", nil, []string{"checks.json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readGeneration(dir); err != nil {
		t.Fatalf("fresh generation rejected: %v", err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Error("the stage directory must be removed")
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGeneration(dir); err == nil {
		t.Fatal("an edited file must make the generation incomplete")
	}
}

// Every error code the package can return must be documented in the README's Errors table.
func TestEveryErrorCodeIsDocumented(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?:newError|traceError)\("([a-z_]+)"`)
	codes := map[string]bool{"review_partial": true, "check_failed": true}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // package source files
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllSubmatch(data, -1) {
			codes[string(match[1])] = true
		}
	}
	for code := range codes {
		if !bytes.Contains(readme, []byte("| `"+code+"` |")) {
			t.Errorf("README Errors table lacks %s", code)
		}
	}
}

func code(err error) string {
	var e *v1.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

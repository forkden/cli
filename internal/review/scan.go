package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Size caps protect the laptop and the source; variables so tests can lower them.
var (
	maxTableBytes  int64   = 256 << 20
	maxTableRows   float64 = 1_000_000
	maxScanRows    int64   = 1_000_000
	maxTotalBytes  int64   = 4 << 30
	maxChangeBytes int64   = 256 << 20
)

const fetchRows = 16384

// userSchemas excludes system schemas; every other schema is in scope.
const userSchemas = `n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'`

type column struct {
	Name      string
	Type      string // format_type with typmod
	Generated bool
	Identity  string // "", "a" (always) or "d" (by default)
}

type table struct {
	Schema   string
	Name     string
	Bytes    int64
	Tuples   float64
	Columns  []column
	PK       []string
	PKTypes  []string // base type names, for the comparator whitelist
	Rows     int64
	SHA256   string
	TooLarge bool
}

func (t *table) key() string { return t.Schema + "." + t.Name }

func (t *table) overCap() bool {
	return t.Bytes > maxTableBytes || t.Tuples > maxTableRows
}

// listTables reads every ordinary table in user schemas with size, columns and primary key.
func listTables(ctx context.Context, tx pgx.Tx) ([]*table, error) {
	rows, err := tx.Query(ctx, `SELECT n.nspname, c.relname, pg_total_relation_size(c.oid), c.reltuples::float8
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND `+userSchemas+`
		ORDER BY n.nspname COLLATE "C", c.relname COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	var tables []*table
	byKey := map[string]*table{}
	for rows.Next() {
		t := &table{}
		if err := rows.Scan(&t.Schema, &t.Name, &t.Bytes, &t.Tuples); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
		byKey[t.key()] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT n.nspname, c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
			a.attgenerated <> '', a.attidentity::text
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped AND `+userSchemas+`
		ORDER BY n.nspname COLLATE "C", c.relname COLLATE "C", a.attnum`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var schema, name string
		var col column
		if err := rows.Scan(&schema, &name, &col.Name, &col.Type, &col.Generated, &col.Identity); err != nil {
			rows.Close()
			return nil, err
		}
		if t := byKey[schema+"."+name]; t != nil {
			t.Columns = append(t.Columns, col)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT n.nspname, c.relname,
			array_agg(a.attname::text ORDER BY k.ord),
			array_agg(format_type(CASE WHEN ty.typtype = 'd' THEN ty.typbasetype ELSE a.atttypid END, NULL) ORDER BY k.ord)
		FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum, ord) ON k.ord <= i.indnkeyatts
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		JOIN pg_type ty ON ty.oid = a.atttypid
		WHERE i.indisprimary AND c.relkind = 'r' AND `+userSchemas+`
		GROUP BY n.nspname, c.relname`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var schema, name string
		var pk, types []string
		if err := rows.Scan(&schema, &name, &pk, &types); err != nil {
			rows.Close()
			return nil, err
		}
		if t := byKey[schema+"."+name]; t != nil {
			t.PK, t.PKTypes = pk, types
		}
	}
	rows.Close()
	return tables, rows.Err()
}

// comparableTypes are primary-key types whose server ordering compareValues reproduces.
var comparableTypes = map[string]bool{
	"smallint": true, "integer": true, "bigint": true, "numeric": true,
	"text": true, "character varying": true, "character": true,
	"uuid": true, "bytea": true, "boolean": true, "date": true,
	"timestamp without time zone": true, "timestamp with time zone": true,
}

var textTypes = map[string]bool{"text": true, "character varying": true, "character": true}

func (t *table) pkComparable() bool {
	for _, typ := range t.PKTypes {
		if !comparableTypes[typ] {
			return false
		}
	}
	return len(t.PK) > 0
}

// orderBy is shared by the fork digest and the diff scan so both read rows in one order.
// Text keys sort bytewise (COLLATE "C"), which Go string comparison reproduces.
func (t *table) orderBy() string {
	if len(t.PK) == 0 {
		return `pg_catalog.row_to_json(t)::text COLLATE "C"`
	}
	parts := make([]string, len(t.PK))
	for i, name := range t.PK {
		parts[i] = quoteIdent("t", name)
		if i < len(t.PKTypes) && textTypes[t.PKTypes[i]] {
			parts[i] += ` COLLATE "C"`
		}
	}
	return strings.Join(parts, ", ")
}

// manifestSQL is a PostgreSQL 12+ port of core's schema manifest over every user schema.
// rel names the owning table for table-scoped records and is NULL otherwise.
const manifestSQL = `WITH s AS (SELECT n.oid, n.nspname FROM pg_namespace n WHERE ` + userSchemas + `),
objects(kind, schema, rel, record) AS (
	SELECT 'database', NULL::text, NULL::text, jsonb_build_array(pg_encoding_to_char(d.encoding), d.datcollate, d.datctype)::text
		FROM pg_database d WHERE d.datname = current_database()
	UNION ALL SELECT 'schema', s.nspname, NULL, jsonb_build_array(s.nspname)::text FROM s
	UNION ALL SELECT 'extension', NULL, NULL, jsonb_build_array(e.extname, e.extversion)::text FROM pg_extension e
	UNION ALL SELECT 'relation', s.nspname, CASE WHEN c.relkind = 'r' THEN c.relname END,
		jsonb_build_array(s.nspname, c.relname, c.relkind, c.relpersistence, c.relreplident)::text
		FROM pg_class c JOIN s ON s.oid = c.relnamespace WHERE c.relkind IN ('r', 'p', 'S', 'c', 'v', 'm', 'f')
	UNION ALL SELECT 'column', s.nspname, c.relname,
		jsonb_build_array(s.nspname, c.relname, a.attname, row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum),
			format_type(a.atttypid, a.atttypmod), a.attnotnull, a.attidentity, a.attgenerated,
			pg_get_expr(ad.adbin, ad.adrelid), coll.collname)::text
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN s ON s.oid = c.relnamespace
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		LEFT JOIN pg_collation coll ON coll.oid = a.attcollation
		WHERE c.relkind IN ('r', 'p', 'c') AND a.attnum > 0 AND NOT a.attisdropped
	UNION ALL SELECT 'constraint', s.nspname, rel.relname,
		jsonb_build_array(s.nspname, rel.relname, ty.typname, c.conname, c.contype, c.convalidated, pg_get_constraintdef(c.oid))::text
		FROM pg_constraint c JOIN s ON s.oid = c.connamespace
		LEFT JOIN pg_class rel ON rel.oid = c.conrelid LEFT JOIN pg_type ty ON ty.oid = c.contypid
	UNION ALL SELECT 'index', s.nspname, tc.relname, jsonb_build_array(s.nspname, ic.relname, pg_get_indexdef(i.indexrelid), i.indisvalid)::text
		FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid JOIN pg_class tc ON tc.oid = i.indrelid JOIN s ON s.oid = tc.relnamespace
	UNION ALL SELECT 'trigger', s.nspname, c.relname, jsonb_build_array(s.nspname, c.relname, tg.tgname, pg_get_triggerdef(tg.oid), tg.tgenabled)::text
		FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid JOIN s ON s.oid = c.relnamespace WHERE NOT tg.tgisinternal
	UNION ALL SELECT 'function', s.nspname, NULL,
		jsonb_build_array(s.nspname, p.proname, pg_get_function_identity_arguments(p.oid),
			CASE WHEN p.prokind IN ('f', 'p', 'w') THEN pg_get_functiondef(p.oid) ELSE p.prokind::text END)::text
		FROM pg_proc p JOIN s ON s.oid = p.pronamespace
	UNION ALL SELECT 'view', s.nspname, NULL, jsonb_build_array(s.nspname, c.relname, c.relkind, pg_get_viewdef(c.oid))::text
		FROM pg_class c JOIN s ON s.oid = c.relnamespace WHERE c.relkind IN ('v', 'm')
	UNION ALL SELECT 'sequence', s.nspname, NULL,
		jsonb_build_array(s.nspname, c.relname, format_type(q.seqtypid, NULL), q.seqstart, q.seqincrement, q.seqmax, q.seqmin, q.seqcache, q.seqcycle)::text
		FROM pg_sequence q JOIN pg_class c ON c.oid = q.seqrelid JOIN s ON s.oid = c.relnamespace
	UNION ALL SELECT 'type', s.nspname, NULL,
		jsonb_build_array(s.nspname, ty.typname, ty.typtype, format_type(ty.typbasetype, ty.typtypmod), ty.typnotnull, ty.typdefault)::text
		FROM pg_type ty JOIN s ON s.oid = ty.typnamespace LEFT JOIN pg_class c ON c.oid = ty.typrelid
		WHERE ty.typtype IN ('d', 'e') OR (ty.typtype = 'c' AND c.relkind = 'c')
	UNION ALL SELECT 'enum', s.nspname, NULL,
		jsonb_build_array(s.nspname, ty.typname, e.enumlabel, row_number() OVER (PARTITION BY ty.oid ORDER BY e.enumsortorder))::text
		FROM pg_enum e JOIN pg_type ty ON ty.oid = e.enumtypid JOIN s ON s.oid = ty.typnamespace
)
SELECT kind, COALESCE(schema, ''), COALESCE(rel, ''), record FROM objects
ORDER BY kind COLLATE "C", COALESCE(schema, '') COLLATE "C", COALESCE(rel, '') COLLATE "C", record COLLATE "C"`

type manifestRecord struct {
	Kind, Schema, Rel, Record string
}

// tableKey is "schema.table" for table-scoped records and "" for everything else.
func (m manifestRecord) tableKey() string {
	if m.Rel == "" {
		return ""
	}
	return m.Schema + "." + m.Rel
}

func readManifest(ctx context.Context, tx pgx.Tx) ([]manifestRecord, error) {
	rows, err := tx.Query(ctx, manifestSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []manifestRecord
	for rows.Next() {
		var record manifestRecord
		if err := rows.Scan(&record.Kind, &record.Schema, &record.Rel, &record.Record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func manifestDigest(records []manifestRecord) string {
	h := sha256.New()
	for _, record := range records {
		// PostgreSQL JSON escapes line breaks, so each record is one unambiguous line.
		_, _ = fmt.Fprintf(h, "%s\t%s\t%s\t%s\n", record.Kind, record.Schema, record.Rel, record.Record)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fingerprint is SHA-256 over the manifest digest plus each in-scope table's content digest.
func fingerprint(manifest string, tables []*table) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "manifest %s\n", manifest)
	for _, t := range tables {
		if t.TooLarge {
			continue
		}
		_, _ = fmt.Fprintf(h, "table %s %d %s\n", strconv.Quote(t.key()), t.Rows, t.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// digestTable hashes every row's row_to_json text in orderBy order (core's digest).
func digestTable(ctx context.Context, tx pgx.Tx, t *table) error {
	writer := digestWriter{hash: sha256.New()}
	query := "DECLARE forkden_review_digest NO SCROLL CURSOR WITHOUT HOLD FOR SELECT pg_catalog.row_to_json(t)::text FROM ONLY " +
		quoteIdent(t.Schema, t.Name) + " AS t ORDER BY " + t.orderBy()
	if _, err := tx.Exec(ctx, query); err != nil {
		return err
	}
	var count int64
	for {
		rows, err := tx.Query(ctx, "FETCH FORWARD "+strconv.Itoa(fetchRows)+" FROM forkden_review_digest", pgx.QueryResultFormats{pgx.TextFormatCode})
		if err != nil {
			return err
		}
		fetched := 0
		for rows.Next() {
			values := rows.RawValues()
			if len(values) != 1 || values[0] == nil {
				rows.Close()
				return errors.New("digest cursor returned an invalid row")
			}
			writer.writeCopyRow(values[0])
			fetched++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		count += int64(fetched)
		if fetched == 0 {
			break
		}
	}
	if _, err := tx.Exec(ctx, "CLOSE forkden_review_digest"); err != nil {
		return err
	}
	t.Rows, t.SHA256 = count, hex.EncodeToString(writer.hash.Sum(nil))
	return nil
}

// shortcut: duplicated from core integrity.go (digestWriter), revisit when a second consumer exists.
type digestWriter struct {
	hash   hash.Hash
	escape [2]byte
}

func (w *digestWriter) writeCopyRow(value []byte) {
	// Byte-for-byte compatible with PostgreSQL COPY text, like core's public-v1 digest.
	w.escape[0] = '\\'
	start := 0
	for i, character := range value {
		switch character {
		case '\\':
			w.escape[1] = '\\'
		case '\b':
			w.escape[1] = 'b'
		case '\f':
			w.escape[1] = 'f'
		case '\n':
			w.escape[1] = 'n'
		case '\r':
			w.escape[1] = 'r'
		case '\t':
			w.escape[1] = 't'
		case '\v':
			w.escape[1] = 'v'
		default:
			continue
		}
		_, _ = w.hash.Write(value[start:i])
		_, _ = w.hash.Write(w.escape[:])
		start = i + 1
	}
	_, _ = w.hash.Write(value[start:])
	_, _ = w.hash.Write([]byte{'\n'})
}

// compareKeys orders two primary-key tuples exactly as the server's ORDER BY does.
func compareKeys(a, b []any) (int, error) {
	if len(a) != len(b) {
		return 0, errors.New("primary key arity differs")
	}
	for i := range a {
		c, err := compareValues(a[i], b[i])
		if err != nil || c != 0 {
			return c, err
		}
	}
	return 0, nil
}

func compareValues(a, b any) (int, error) {
	switch x := a.(type) {
	case int16, int32, int64:
		left, lok := asInt(a)
		right, rok := asInt(b)
		if !lok || !rok {
			break
		}
		return cmpOrdered(left, right), nil
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y), nil
		}
	case []byte:
		if y, ok := b.([]byte); ok {
			return bytes.Compare(x, y), nil
		}
	case [16]byte:
		if y, ok := b.([16]byte); ok {
			return bytes.Compare(x[:], y[:]), nil
		}
	case bool:
		if y, ok := b.(bool); ok {
			return cmpOrdered(boolInt(x), boolInt(y)), nil
		}
	case pgtype.Numeric:
		if y, ok := b.(pgtype.Numeric); ok {
			return compareNumeric(x, y), nil
		}
	case time.Time, pgtype.InfinityModifier:
		left, lok := timeRank(a)
		right, rok := timeRank(b)
		if lok && rok {
			if left != right || left != 0 {
				return cmpOrdered(left, right), nil
			}
			return a.(time.Time).Compare(b.(time.Time)), nil
		}
	}
	return 0, fmt.Errorf("cannot compare primary key values of types %T and %T", a, b)
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	}
	return 0, false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func cmpOrdered[T int | int64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// timeRank is -1 for -infinity, 0 for a finite time and 1 for infinity.
func timeRank(v any) (int, bool) {
	switch x := v.(type) {
	case time.Time:
		return 0, true
	case pgtype.InfinityModifier:
		return int(x), true
	}
	return 0, false
}

// compareNumeric follows PostgreSQL: -Infinity < finite < Infinity < NaN.
func compareNumeric(a, b pgtype.Numeric) int {
	rank := func(n pgtype.Numeric) int {
		switch {
		case n.NaN:
			return 2
		case n.InfinityModifier == pgtype.Infinity:
			return 1
		case n.InfinityModifier == pgtype.NegativeInfinity:
			return -1
		}
		return 0
	}
	if ra, rb := rank(a), rank(b); ra != rb || ra != 0 {
		return cmpOrdered(ra, rb)
	}
	return numericRat(a).Cmp(numericRat(b))
}

func numericRat(n pgtype.Numeric) *big.Rat {
	value := new(big.Rat).SetInt(n.Int)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt(n.Exp))), nil)
	if n.Exp >= 0 {
		return value.Mul(value, new(big.Rat).SetInt(scale))
	}
	return value.Quo(value, new(big.Rat).SetInt(scale))
}

func absInt(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func sortTables(tables []*table) {
	slices.SortFunc(tables, func(a, b *table) int { return strings.Compare(a.key(), b.key()) })
}

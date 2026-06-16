package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Query runs query and scans every row into a T. Returns a (possibly
// empty) slice on success. T must be a struct.
//
// args are normalized via [argOf] before being passed to the driver,
// so a `time.Time` bind value compares correctly against rows
// inserted by this library.
func Query[T any](ctx context.Context, db Executor, query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, normArgs(args)...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	out := []T{}
	for rows.Next() {
		var item T
		if err := scanRow(rows, &item); err != nil {
			return nil, classify(err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// QueryOne runs query and scans exactly one row into a T. Returns
// [ErrNotFound] when the query produces no rows.
//
// args are normalized via [argOf] before being passed to the driver,
// so a `time.Time` bind value compares correctly against rows
// inserted by this library.
func QueryOne[T any](ctx context.Context, db Executor, query string, args ...any) (T, error) {
	var zero T
	rows, err := db.QueryContext(ctx, query, normArgs(args)...)
	if err != nil {
		return zero, classify(err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, classify(err)
		}
		return zero, ErrNotFound
	}
	var out T
	if err := scanRow(rows, &out); err != nil {
		return zero, classify(err)
	}
	return out, nil
}

// scanRow reads one row from rows into *dst. The caller has already
// confirmed rows.Next() returned true.
func scanRow[T any](rows *sql.Rows, dst *T) error {
	typ := reflect.TypeOf(*dst)
	// typ is nil when T is an interface type (e.g. Query[any]): the
	// zero value is a nil interface with no dynamic type. Guard before
	// the Kind() call, which would otherwise nil-deref.
	if typ == nil || typ.Kind() != reflect.Struct {
		return fmt.Errorf("sqlx: T must be a struct, got %s", reflect.TypeOf(dst).Elem())
	}

	cols, err := rows.Columns()
	if err != nil {
		return err
	}

	// Reject ambiguous result columns. The common cause is a JOIN
	// that selects same-named columns from two tables: SELECT
	// users.id, posts.id … → rows.Columns() is ["id", "id"]. Falling
	// through would silently scan one of them twice into whichever
	// struct field maps to "id". Comparison is case-insensitive so
	// "id" and "ID" (which match the same field) are caught too.
	seen := map[string]int{}
	for i, c := range cols {
		lc := strings.ToLower(c)
		if prev, dup := seen[lc]; dup {
			return fmt.Errorf(
				"sqlx: scan into %s: column %q appears at positions %d and %d in the result; "+
					"alias one with `AS new_name` and add a matching field "+
					"(or `db:\"new_name\"` tag) on the struct",
				typ.Name(), c, prev, i)
		}
		seen[lc] = i
	}

	fm, err := getFieldMap(typ)
	if err != nil {
		return err
	}
	val := reflect.ValueOf(dst).Elem()

	targets := make([]any, len(cols))
	for i, col := range cols {
		idx, ok := fm.colToField[strings.ToLower(col)]
		if !ok {
			// No matching field — scan into a discard.
			var discard any
			targets[i] = &discard
			continue
		}
		fv := val.Field(idx)
		switch fv.Type() {
		case timeType:
			// database/sql cannot scan string → *time.Time. Many
			// drivers (modernc.org/sqlite for one) return time as
			// TEXT. The adapter parses RFC3339[Nano] on the way in.
			targets[i] = &timeScanner{dst: fv.Addr().Interface().(*time.Time)}
		case ptrTimeType:
			// Nullable time: NULL → nil, value → allocated.
			targets[i] = &ptrTimeScanner{dst: fv.Addr().Interface().(**time.Time)}
		default:
			targets[i] = fv.Addr().Interface()
		}
	}
	return rows.Scan(targets...)
}

var (
	timeType    = reflect.TypeOf(time.Time{})
	ptrTimeType = reflect.TypeOf((*time.Time)(nil))
	valuerType  = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
	scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
)

// isColumnStruct reports whether a struct type t can still serve as a
// single column. time.Time is special-cased throughout the library;
// otherwise the type must implement database/sql's Valuer or Scanner
// (checked on both the value and pointer types, since Scan usually
// has a pointer receiver) — that covers [sql.Null], [JSON], and any
// user-defined column type. A plain data struct implements none of
// these and cannot round-trip as one column.
func isColumnStruct(t reflect.Type) bool {
	if t == timeType {
		return true
	}
	pt := reflect.PointerTo(t)
	return t.Implements(valuerType) || pt.Implements(valuerType) ||
		t.Implements(scannerType) || pt.Implements(scannerType)
}

// argOf normalizes one value for binding to a SQL placeholder. The
// special cases are time.Time and *time.Time: drivers differ on how
// to encode them (modernc/sqlite uses Go's default String format
// which is not parseable back; pgx uses driver-native TIMESTAMP).
// The library forces RFC3339Nano TEXT so the round-trip is
// deterministic across backends, and — critically — so that the
// format the library writes matches the format it uses for
// WHERE-clause comparisons.
//
// *time.Time: nil → nil (the driver writes NULL), non-nil → the
// pointer is dereferenced and the time is formatted as if it were a
// value time.Time.
//
// argOf is applied at every entry point that hits the driver:
// Insert / InsertMany row values, Update row values + whereArgs,
// Delete whereArgs, Query / QueryOne args.
func argOf(v any) any {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if t == nil {
			return nil
		}
		// Dereference and reuse the value path so zero handling stays
		// symmetric with time.Time (a zero time → "", not the
		// "0001-01-01T00:00:00Z" literal).
		return argOf(*t)
	}
	return v
}

// normArgs returns a normalized copy of args, applying [argOf] to
// each entry. Returns args unchanged when the input is empty so
// callers do not allocate for the no-args case.
func normArgs(args []any) []any {
	if len(args) == 0 {
		return args
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = argOf(a)
	}
	return out
}

// timeScanner adapts string / []byte / time.Time / nil source values
// into a *time.Time destination. Drivers vary: pgx returns time.Time,
// modernc/sqlite returns string (RFC3339Nano), mattn/sqlite3 returns
// time.Time when the column type contains "DATE" or "TIME".
type timeScanner struct{ dst *time.Time }

func (s *timeScanner) Scan(src any) error {
	switch v := src.(type) {
	case time.Time:
		*s.dst = v
		return nil
	case string:
		return s.parse(v)
	case []byte:
		return s.parse(string(v))
	case nil:
		*s.dst = time.Time{}
		return nil
	default:
		return fmt.Errorf("sqlx: cannot scan %T into time.Time", src)
	}
}

func (s *timeScanner) parse(raw string) error {
	if raw == "" {
		*s.dst = time.Time{}
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			*s.dst = t
			return nil
		}
	}
	return fmt.Errorf("sqlx: time.Time scan: cannot parse %q", raw)
}

// ptrTimeScanner adapts the same source values as [timeScanner] into
// a **time.Time destination so nullable time columns round-trip with
// `*time.Time` struct fields: NULL becomes a nil pointer; a value
// becomes a freshly allocated *time.Time.
type ptrTimeScanner struct{ dst **time.Time }

func (s *ptrTimeScanner) Scan(src any) error {
	if src == nil {
		*s.dst = nil
		return nil
	}
	var t time.Time
	inner := timeScanner{dst: &t}
	if err := inner.Scan(src); err != nil {
		return err
	}
	*s.dst = &t
	return nil
}

// fieldMap is the per-T snapshot of struct-field metadata: which
// fields exist in declaration order (for Insert / Update column
// lists) and which column name maps to which field index (for
// Scan).
type fieldMap struct {
	// fields in declaration order, with column name + index.
	fields []fieldInfo
	// colToField is the reverse: lowercased column name →
	// fields[].index. Keyed lowercase so scan matching is
	// case-insensitive, regardless of the case a driver reports.
	colToField map[string]int
	// pkIndex is the field index of the conventional auto-PK
	// (column "id" — derived field name "ID" or `db:"id"`). -1 if
	// the struct has no such field.
	pkIndex int
}

type fieldInfo struct {
	index  int    // index into reflect.Value's Field()
	column string // database column name
}

// fieldMapEntry is the cached result of building a type's field map.
// Both the map and the build error are memoized: the outcome is
// deterministic per type, so a misconfigured struct reports the same
// error on every use without rebuilding.
type fieldMapEntry struct {
	fm  *fieldMap
	err error
}

var fieldMapCache sync.Map // reflect.Type → fieldMapEntry

// getFieldMap returns the cached *fieldMap for typ, building it on
// first use. typ must be a struct. It returns an error when the
// struct's fields cannot produce a valid map — a column name that is
// not a valid SQL identifier ([ErrInvalidIdentifier]) or two fields
// resolving to the same column ([ErrDuplicateColumn]).
func getFieldMap(typ reflect.Type) (*fieldMap, error) {
	if v, ok := fieldMapCache.Load(typ); ok {
		e := v.(fieldMapEntry)
		return e.fm, e.err
	}
	fm, err := buildFieldMap(typ)
	actual, _ := fieldMapCache.LoadOrStore(typ, fieldMapEntry{fm: fm, err: err})
	e := actual.(fieldMapEntry)
	return e.fm, e.err
}

func buildFieldMap(typ reflect.Type) (*fieldMap, error) {
	fm := &fieldMap{
		colToField: map[string]int{},
		pkIndex:    -1,
	}
	// colOwner records the field name that first claimed a column,
	// keyed by the lowercased column name. Used to produce a helpful
	// error message on collision. Column matching on scan is
	// case-insensitive, so columns differing only in case collide
	// here too. (Unquoted SQL identifiers are case-insensitive on
	// every backend; PostgreSQL treats *quoted* identifiers as
	// case-sensitive — "Order" ≠ "order" — so two such columns that
	// are distinct on Postgres would be reported as ErrDuplicateColumn
	// here. A deliberate simplification of the case-insensitive model.)
	colOwner := map[string]string{}

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		col := f.Tag.Get("db")
		if col == "-" {
			// Explicit skip, like encoding/json's `json:"-"`: the
			// field is neither written nor scanned.
			continue
		}
		// Reject a struct-typed field (embedded or named) that
		// database/sql can't treat as a single column. The library
		// does not flatten nested structs, so such a field would
		// otherwise bind as one opaque arg on writes and silently read
		// back zero. time.Time and Valuer/Scanner types are fine.
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !isColumnStruct(ft) {
			return nil, fmt.Errorf(
				"%w: type %s.%s field %s (%s) is a struct that is neither "+
					"time.Time nor a database/sql Valuer/Scanner — flatten it "+
					"into scalar fields, implement driver.Valuer + sql.Scanner, "+
					"or skip it with `db:\"-\"`",
				ErrUnsupportedFieldType, typ.PkgPath(), typ.Name(), f.Name, f.Type)
		}
		if col == "" {
			col = snakeCase(f.Name)
		}
		if !validIdentifier(col) {
			return nil, fmt.Errorf(
				"%w: type %s.%s field %s maps to column %q (column names are "+
					"interpolated, not parameterized — fix the field name or its "+
					"`db:` tag)",
				ErrInvalidIdentifier, typ.PkgPath(), typ.Name(), f.Name, col)
		}
		// Index by the unquoted, lowercased name so scan matching is
		// case-insensitive and matches what a driver reports for a
		// quoted column (declared "Order" → reported Order). The
		// original col (quotes and case intact) stays in
		// fieldInfo.column for the INSERT / UPDATE emit path.
		key := strings.ToLower(unquoteIdent(col))
		if prev, exists := colOwner[key]; exists {
			return nil, fmt.Errorf(
				"%w: type %s.%s column %q from fields %s and %s "+
					"(check snake_case derived names and `db:` tag overrides)",
				ErrDuplicateColumn, typ.PkgPath(), typ.Name(), col, prev, f.Name)
		}
		colOwner[key] = f.Name
		fm.fields = append(fm.fields, fieldInfo{index: i, column: col})
		fm.colToField[key] = i
		if key == "id" {
			fm.pkIndex = len(fm.fields) - 1
		}
	}
	return fm, nil
}

// snakeCase converts a Go field name to a SQL column name.
// Examples: UserID → user_id, URL → url, HTTPRequest → http_request,
// IDList → id_list, alreadyLower → already_lower.
//
// Underscore insertion and lowercasing apply to ASCII letters only;
// any non-ASCII rune is passed through unchanged (as its full UTF-8
// bytes, not truncated). Such a column then fails [validIdentifier],
// so a non-ASCII field name surfaces as ErrInvalidIdentifier rather
// than silently mapping to a wrong column.
func snakeCase(name string) string {
	if name == "" {
		return ""
	}
	buf := make([]byte, 0, len(name)+4)
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				prev := name[i-1]
				switch {
				case prev >= 'a' && prev <= 'z':
					// lower → upper boundary
					buf = append(buf, '_')
				case prev >= 'A' && prev <= 'Z' && i+1 < len(name) && isLower(name[i+1]):
					// end of caps run, next is lower (HTTPRequest → http_request)
					buf = append(buf, '_')
				}
			}
			buf = append(buf, byte(r-'A'+'a'))
		default:
			// Append the rune's full UTF-8 encoding. For ASCII this is
			// the single byte; for non-ASCII it preserves all bytes
			// instead of truncating with byte(r).
			buf = append(buf, string(r)...)
		}
	}
	return string(buf)
}

func isLower(b byte) bool { return b >= 'a' && b <= 'z' }

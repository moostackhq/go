package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pure-Go tests for parts that don't need a SQL backend. The main
// behavioral test suite lives in sqlx/sqlite/sqlite_test.go where
// it can run against an in-memory SQLite database.

func TestSnakeCase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ID", "id"},
		{"UserID", "user_id"},
		{"URL", "url"},
		{"HTTPRequest", "http_request"},
		{"alreadyLower", "already_lower"},
		{"IDList", "id_list"},
		{"X", "x"},
		{"", ""},
		{"PostID", "post_id"},
		{"CreatedAt", "created_at"},
		// Non-ASCII runes pass through as full UTF-8, not truncated to
		// a single byte. "š" (U+0161) must NOT become "a" (its low
		// byte, 0x61) — regression for the silent-corruption bug.
		{"š", "š"},
		{"café", "café"},
	}
	for _, c := range cases {
		got := snakeCase(c.in)
		if got != c.want {
			t.Errorf("snakeCase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFieldMap_BuiltOncePerType(t *testing.T) {
	type User struct {
		ID    int64
		Email string
		Name  string `db:"display_name"`
	}
	fm1, err := getFieldMap(reflect.TypeOf(User{}))
	if err != nil {
		t.Fatalf("getFieldMap: unexpected error %v", err)
	}
	fm2, _ := getFieldMap(reflect.TypeOf(User{}))
	if fm1 != fm2 {
		t.Errorf("fieldMap not cached: got distinct pointers %p vs %p", fm1, fm2)
	}
	if fm1.pkIndex != 0 || fm1.fields[0].column != "id" {
		t.Errorf("PK not detected: pkIndex=%d, first column=%q", fm1.pkIndex, fm1.fields[0].column)
	}
	if got := fm1.colToField["display_name"]; got != 2 {
		t.Errorf("db tag override not honored: colToField[display_name]=%d, want 2", got)
	}
}

func TestFieldMap_SkipsUnexported(t *testing.T) {
	type withPrivate struct {
		ID     int64
		Public string
		hidden string //nolint:unused // intentional unexported field for test
	}
	fm, err := getFieldMap(reflect.TypeOf(withPrivate{}))
	if err != nil {
		t.Fatalf("getFieldMap: unexpected error %v", err)
	}
	if len(fm.fields) != 2 {
		t.Errorf("expected 2 exported fields, got %d", len(fm.fields))
	}
}

// TestFieldMap_DuplicateColumnError asserts that two struct fields
// resolving to the same column (via `db:` tag override or accidental
// snake_case collision) return ErrDuplicateColumn at first use of the
// type, with a message naming both fields. Struct-definition error —
// surfaced from the helper, not a panic.
func TestFieldMap_DuplicateColumnError(t *testing.T) {
	type T struct {
		Alpha string `db:"x"`
		Beta  string `db:"x"`
	}
	// Use a unique type so the cache doesn't return a stale entry.
	_, err := getFieldMap(reflect.TypeOf(T{}))
	if !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("error = %v, want ErrDuplicateColumn", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "duplicate column") {
		t.Errorf("error %q does not say %q", msg, "duplicate column")
	}
	if !strings.Contains(msg, "Alpha") || !strings.Contains(msg, "Beta") {
		t.Errorf("error %q does not name both colliding fields Alpha and Beta", msg)
	}
	if !strings.Contains(msg, `"x"`) {
		t.Errorf("error %q does not name the collided column", msg)
	}
}

// TestFieldMap_TagCollidesWithDerivedName covers the cross-collision:
// one field's snake_case derivation collides with another's `db:` tag.
func TestFieldMap_TagCollidesWithDerivedName(t *testing.T) {
	type T struct {
		UserID int64  // derives to "user_id"
		Other  string `db:"user_id"` // explicit override collides
	}
	if _, err := getFieldMap(reflect.TypeOf(T{})); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("error = %v, want ErrDuplicateColumn when db: tag collides with derived name", err)
	}
}

// TestFieldMap_CaseOnlyDuplicateColumnError documents that columns
// differing only in case collide: scan matching is case-insensitive,
// so two fields mapping to "Name"/"name" would be ambiguous.
func TestFieldMap_CaseOnlyDuplicateColumnError(t *testing.T) {
	type T struct {
		A string `db:"Name"`
		B string `db:"name"`
	}
	if _, err := getFieldMap(reflect.TypeOf(T{})); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("error = %v, want ErrDuplicateColumn for a case-only column collision", err)
	}
}

// TestFieldMap_DashTagSkipsField verifies `db:"-"` excludes a field
// from the column set entirely (like encoding/json): it appears in
// neither the field list nor the column map, and is never emitted in
// an INSERT.
func TestFieldMap_DashTagSkipsField(t *testing.T) {
	type T struct {
		ID       int64
		Email    string
		Internal string `db:"-"`
	}
	fm, err := getFieldMap(reflect.TypeOf(T{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(fm.fields) != 2 {
		t.Errorf("fields = %d, want 2 (Internal skipped)", len(fm.fields))
	}
	if _, ok := fm.colToField["internal"]; ok {
		t.Error("skipped field 'Internal' present in colToField")
	}
	if _, ok := fm.colToField["-"]; ok {
		t.Error("'-' present as a column key")
	}

	exec := &fakeExecutor{}
	if _, err := Insert(context.Background(), exec, "t", T{Email: "a@x", Internal: "secret"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exec.queries[0], "internal") {
		t.Errorf("INSERT names the skipped column: %q", exec.queries[0])
	}
	for _, a := range exec.args[0] {
		if a == "secret" {
			t.Errorf("INSERT bound the skipped field's value: args=%v", exec.args[0])
		}
	}
}

// moneyValuerScanner is a user-defined column type: a struct that
// implements driver.Valuer + sql.Scanner, so it must be accepted as a
// single column despite being a struct.
type moneyValuerScanner struct{ cents int64 }

func (m moneyValuerScanner) Value() (driver.Value, error) { return m.cents, nil }
func (m *moneyValuerScanner) Scan(v any) error {
	if n, ok := v.(int64); ok {
		m.cents = n
	}
	return nil
}

// TestFieldMap_RejectsUnsupportedStructFields verifies that a struct
// field that database/sql can't treat as one column — a plain data
// struct, embedded or named — is rejected loudly with
// ErrUnsupportedFieldType instead of binding opaquely / silently
// reading back zero, while legitimate column-type structs are
// accepted.
func TestFieldMap_RejectsUnsupportedStructFields(t *testing.T) {
	type Address struct{ City, Zip string }
	type Base struct {
		ID        int64
		CreatedAt time.Time
	}

	t.Run("embedded plain struct", func(t *testing.T) {
		type User struct {
			Base
			Email string
		}
		if _, err := getFieldMap(reflect.TypeOf(User{})); !errors.Is(err, ErrUnsupportedFieldType) {
			t.Fatalf("err = %v, want ErrUnsupportedFieldType for embedded struct", err)
		}
	})
	t.Run("named nested plain struct", func(t *testing.T) {
		type User struct {
			Email string
			Addr  Address
		}
		if _, err := getFieldMap(reflect.TypeOf(User{})); !errors.Is(err, ErrUnsupportedFieldType) {
			t.Fatalf("err = %v, want ErrUnsupportedFieldType for nested struct", err)
		}
	})
	t.Run("pointer to plain struct", func(t *testing.T) {
		type User struct {
			Email string
			Addr  *Address
		}
		if _, err := getFieldMap(reflect.TypeOf(User{})); !errors.Is(err, ErrUnsupportedFieldType) {
			t.Fatalf("err = %v, want ErrUnsupportedFieldType for *struct", err)
		}
	})
	t.Run("db:- skips an embedded struct", func(t *testing.T) {
		type User struct {
			Base  `db:"-"`
			Email string
		}
		fm, err := getFieldMap(reflect.TypeOf(User{}))
		if err != nil {
			t.Fatalf(`db:"-" embedded struct should be skipped, got %v`, err)
		}
		if len(fm.fields) != 1 || fm.fields[0].column != "email" {
			t.Errorf("fields = %+v, want only email", fm.fields)
		}
	})
	t.Run("accepts column-type structs", func(t *testing.T) {
		type Row struct {
			ID      int64
			Name    sql.Null[string]
			Meta    JSON[map[string]any]
			At      time.Time
			Balance moneyValuerScanner
		}
		if _, err := getFieldMap(reflect.TypeOf(Row{})); err != nil {
			t.Fatalf("column-type structs must be accepted, got %v", err)
		}
	})
}

// TestPkAsInt64 pins the PK→int64 conversion, including the overflow
// case: a uint64 beyond MaxInt64 has no faithful int64, so it returns
// 0 (the "caller already has the id" sentinel) rather than wrapping
// negative.
func TestPkAsInt64(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want int64
	}{
		{"int64", int64(42), 42},
		{"int32", int32(-7), -7},
		{"uint32 max", uint32(math.MaxUint32), int64(math.MaxUint32)},
		{"uint64 in range", uint64(123), 123},
		{"uint64 == MaxInt64", uint64(math.MaxInt64), math.MaxInt64},
		{"uint64 over MaxInt64", uint64(math.MaxUint64), 0},
		{"string PK", "uuid-abc", 0},
	}
	for _, c := range cases {
		if got := pkAsInt64(reflect.ValueOf(c.v)); got != c.want {
			t.Errorf("%s: pkAsInt64(%v) = %d, want %d", c.name, c.v, got, c.want)
		}
	}
}

// TestJSON_ValueMarshalError verifies that an unmarshalable payload
// surfaces as an error from Value() rather than panicking — the path
// database/sql takes when binding the arg.
func TestJSON_ValueMarshalError(t *testing.T) {
	j := JSON[chan int]{V: make(chan int)} // chan is not JSON-marshalable
	if _, err := j.Value(); err == nil {
		t.Fatal("JSON.Value() with unmarshalable payload = nil error, want an error (no panic)")
	}
}

// TestInsertMany_RejectsNonStructElements directly asserts that a
// slice whose element type is not a struct — pointer-to-struct or
// interface — is rejected with no query issued.
func TestInsertMany_RejectsNonStructElements(t *testing.T) {
	type Row struct {
		ID   int64
		Name string
	}
	t.Run("pointer elements", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, "t", []*Row{{Name: "x"}})
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want 'slice element must be a struct'", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
	t.Run("interface elements", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, "t", []any{Row{Name: "x"}})
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want 'slice element must be a struct'", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
}

// TestFieldMap_NonASCIIFieldRejected verifies that a non-ASCII field
// name fails loudly. "Ł" (U+0141) is the dangerous case: its low byte
// is 0x41 ('A'), so the old byte-truncating snakeCase silently mapped
// it to column "a"; now the full rune is preserved and validIdentifier
// rejects it with ErrInvalidIdentifier.
func TestFieldMap_NonASCIIFieldRejected(t *testing.T) {
	type T struct {
		Ł string
	}
	if _, err := getFieldMap(reflect.TypeOf(T{})); !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("error = %v, want ErrInvalidIdentifier for a non-ASCII field name", err)
	}
}

// --- fakeExecutor: minimal Executor for driver-independent tests --

type fakeExecutor struct {
	queries []string
	args    [][]any
	lastID  int64
}

func (e *fakeExecutor) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	e.queries = append(e.queries, q)
	e.args = append(e.args, args)
	return fakeResult{lastID: e.lastID}, nil
}
func (e *fakeExecutor) QueryContext(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, errors.New("fakeExecutor: QueryContext unused")
}
func (e *fakeExecutor) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	return nil
}

type fakeResult struct{ lastID int64 }

func (r fakeResult) LastInsertId() (int64, error) { return r.lastID, nil }
func (r fakeResult) RowsAffected() (int64, error) { return 0, nil }

// TestInsert_SuppliedPKReturnedNotLastInsertID is the regression for
// the bug where Insert always returned LastInsertId, dropping the
// caller-supplied id on drivers that return 0 from LastInsertId
// (pgx is the common case). The fake executor models such a driver
// by returning 0; the test asserts Insert still returns the
// caller's 42.
func TestInsert_SuppliedPKReturnedNotLastInsertID(t *testing.T) {
	type User struct {
		ID    int64
		Email string
	}
	exec := &fakeExecutor{lastID: 0} // mimic a driver without LastInsertId
	id, err := Insert(context.Background(), exec, "users", User{ID: 42, Email: "a@x"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("Insert returned %d, want 42 (caller-supplied PK must round-trip on drivers without LastInsertId)", id)
	}
	// And the INSERT must have included the id column.
	if !strings.Contains(exec.queries[0], "(id, email)") {
		t.Errorf("INSERT column list = %q, want includes (id, email)", exec.queries[0])
	}
}

// TestInsert_ZeroPKFallsBackToLastInsertID verifies the
// auto-assigned branch still works: when the caller passes a zero
// id, the id column is omitted from the INSERT and the return is
// the driver's LastInsertId.
func TestInsert_ZeroPKFallsBackToLastInsertID(t *testing.T) {
	type User struct {
		ID    int64
		Email string
	}
	exec := &fakeExecutor{lastID: 99}
	id, err := Insert(context.Background(), exec, "users", User{Email: "a@x"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 99 {
		t.Errorf("Insert returned %d, want 99 (LastInsertId from fake driver)", id)
	}
	if strings.Contains(exec.queries[0], "id") {
		t.Errorf("INSERT column list unexpectedly includes id: %q", exec.queries[0])
	}
}

// TestInsert_NonIntegerPKReturnsZero documents the contract for
// non-numeric PKs (string UUIDs etc.): the caller already knows the
// id, the int64 return cannot represent it, so Insert returns 0.
func TestInsert_NonIntegerPKReturnsZero(t *testing.T) {
	type Event struct {
		ID   string `db:"id"`
		Body string
	}
	exec := &fakeExecutor{lastID: 0}
	id, err := Insert(context.Background(), exec, "events", Event{ID: "uuid-abc", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 0 {
		t.Errorf("Insert with string PK returned %d, want 0", id)
	}
	if !strings.Contains(exec.queries[0], "(id, body)") {
		t.Errorf("INSERT column list = %q, want includes (id, body)", exec.queries[0])
	}
}

// TestInsert_PKOnlyZero_DefaultValues covers issue #10: a struct
// whose only field is a zero-value auto-PK must emit the portable
// "INSERT INTO t DEFAULT VALUES" — not "INSERT INTO t () VALUES ()",
// which PostgreSQL rejects. The DB assigns the id, so the return is
// LastInsertId.
func TestInsert_PKOnlyZero_DefaultValues(t *testing.T) {
	type Row struct{ ID int64 }
	exec := &fakeExecutor{lastID: 7}
	id, err := Insert(context.Background(), exec, "t", Row{})
	if err != nil {
		t.Fatal(err)
	}
	if id != 7 {
		t.Errorf("id = %d, want 7 (LastInsertId)", id)
	}
	q := exec.queries[0]
	if !strings.Contains(q, "DEFAULT VALUES") {
		t.Errorf("query = %q, want DEFAULT VALUES form", q)
	}
	if strings.Contains(q, "()") {
		t.Errorf("query = %q, must not emit empty () VALUES ()", q)
	}
}

// TestInsert_PKOnlyNonZero_IncludesColumn confirms the DEFAULT VALUES
// path is taken only when the column list is empty: a supplied PK
// still inserts the id column normally.
func TestInsert_PKOnlyNonZero_IncludesColumn(t *testing.T) {
	type Row struct{ ID int64 }
	exec := &fakeExecutor{}
	id, err := Insert(context.Background(), exec, "t", Row{ID: 5})
	if err != nil {
		t.Fatal(err)
	}
	if id != 5 {
		t.Errorf("id = %d, want 5 (supplied PK)", id)
	}
	q := exec.queries[0]
	if strings.Contains(q, "DEFAULT VALUES") {
		t.Errorf("query = %q, should insert the supplied id, not DEFAULT VALUES", q)
	}
	if !strings.Contains(q, "(id)") {
		t.Errorf("query = %q, want (id) column list", q)
	}
}

// TestInsertMany_PKOnlyZero_Errors covers the InsertMany sibling of
// issue #10: a batch where every row would insert only an
// auto-assigned id has no portable multi-row form, so it returns
// ErrNoColumns rather than panicking on the empty column list.
func TestInsertMany_PKOnlyZero_Errors(t *testing.T) {
	type Row struct{ ID int64 }
	exec := &fakeExecutor{}
	err := InsertMany(context.Background(), exec, "t", []Row{{}, {}})
	if !errors.Is(err, ErrNoColumns) {
		t.Fatalf("error = %v, want ErrNoColumns", err)
	}
	if len(exec.queries) != 0 {
		t.Fatalf("executed %d queries, want 0", len(exec.queries))
	}
}

// TestWriteHelpers_RejectEmptyStruct asserts that a row struct with
// no exported fields returns ErrNoColumns and issues no query, rather
// than letting a malformed statement (empty SET clause / empty column
// list) reach the driver. InsertMany additionally would panic on the
// empty column list without the guard.
func TestWriteHelpers_RejectEmptyStruct(t *testing.T) {
	type empty struct{}
	type onlyPrivate struct {
		secret int //nolint:unused // only unexported fields → no columns
	}

	t.Run("Insert", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Insert(context.Background(), exec, "users", empty{})
		assertNoColumns(t, exec, err)
	})
	t.Run("InsertMany", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, "users", []empty{{}})
		assertNoColumns(t, exec, err)
	})
	t.Run("Update", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, "users", "id = ?", empty{}, 1)
		assertNoColumns(t, exec, err)
	})
	t.Run("Update_onlyUnexported", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, "users", "id = ?", onlyPrivate{}, 1)
		assertNoColumns(t, exec, err)
	})
}

func assertNoColumns(t *testing.T, exec *fakeExecutor, err error) {
	t.Helper()
	if !errors.Is(err, ErrNoColumns) {
		t.Fatalf("error = %v, want ErrNoColumns", err)
	}
	if len(exec.queries) != 0 {
		t.Fatalf("executed %d queries, want 0: %q", len(exec.queries), exec.queries)
	}
}

// TestWriteHelpers_NilRow ensures a nil row (untyped nil through any)
// returns the "must be a struct" error rather than panicking — the
// write-path analog of the scanRow nil/interface guard. reflect.TypeOf
// of a nil interface is nil, so .Kind() would otherwise nil-deref.
func TestWriteHelpers_NilRow(t *testing.T) {
	t.Run("Insert", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Insert(context.Background(), exec, "t", nil)
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want a 'must be a struct' error (no panic)", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
	t.Run("Update", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, "t", "id = ?", nil, 1)
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want a 'must be a struct' error (no panic)", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
	t.Run("InsertMany", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, "t", nil)
		if err == nil {
			t.Fatal("err = nil, want an error (no panic)")
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
}

// TestWriteHelpers_PointerToStructRejected documents the current
// contract: a pointer-to-struct row is not dereferenced — its Kind is
// Pointer, not Struct — so Insert/Update return the "must be a struct"
// error and issue no query. Pass the struct by value.
func TestWriteHelpers_PointerToStructRejected(t *testing.T) {
	type Row struct {
		ID    int64
		Email string
	}
	t.Run("Insert", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Insert(context.Background(), exec, "t", &Row{Email: "x"})
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want 'must be a struct' for *struct", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
	t.Run("Update", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, "t", "id = ?", &Row{Email: "x"}, 1)
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want 'must be a struct' for *struct", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
}

// TestUpdate_WritesZeroPKInSet pins the documented "Update sets every
// column" behavior: unlike Insert (which omits a zero auto-PK), Update
// emits id = ? even when ID is the zero value, binding 0. Reusing an
// insert-style struct with a zero ID therefore sets id = 0 — the
// asymmetry users must know about.
func TestUpdate_WritesZeroPKInSet(t *testing.T) {
	type Row struct {
		ID    int64
		Email string
	}
	exec := &fakeExecutor{}
	if _, err := Update(context.Background(), exec, "t", "email = ?", Row{Email: "x"}, "old@x"); err != nil {
		t.Fatal(err)
	}
	q := exec.queries[0]
	if !strings.Contains(q, "id = ?") {
		t.Errorf("UPDATE query = %q, want it to set id = ? (Update writes every column, incl. a zero PK)", q)
	}
	// SET args come first, in field order: id, email; then whereArgs.
	if exec.args[0][0] != int64(0) {
		t.Errorf("first SET arg = %v (%T), want int64(0) — zero PK written verbatim", exec.args[0][0], exec.args[0][0])
	}
}

// TestArgOf_TimeNormalization pins the time.Time / *time.Time binding
// rules, including the symmetry fix: a non-nil *time.Time at the zero
// time must normalize to "" exactly like a value zero time.Time, not
// to the "0001-01-01T00:00:00Z" literal. A nil pointer still maps to
// SQL NULL.
func TestArgOf_TimeNormalization(t *testing.T) {
	zero := time.Time{}
	stamp := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	want := stamp.Format(time.RFC3339Nano)

	if got := argOf(zero); got != "" {
		t.Errorf("argOf(zero time.Time) = %v, want \"\"", got)
	}
	if got := argOf(&zero); got != "" {
		t.Errorf("argOf(*time.Time at zero) = %v, want \"\" (must match value zero)", got)
	}
	var nilPtr *time.Time
	if got := argOf(nilPtr); got != nil {
		t.Errorf("argOf(nil *time.Time) = %v, want nil (SQL NULL)", got)
	}
	if got := argOf(stamp); got != want {
		t.Errorf("argOf(stamp) = %v, want %q", got, want)
	}
	if got := argOf(&stamp); got != want {
		t.Errorf("argOf(&stamp) = %v, want %q (must match value)", got, want)
	}
}

// TestBindArgNormalization_ExecEntryPoints asserts that argOf is
// applied to time.Time values at every ExecContext-based entry point
// — Insert/InsertMany row values, Update SET values, and the where
// args of Update/Delete — so a raw time.Time never reaches the
// driver. (Query/QueryOne arg normalization is covered end-to-end by
// the SQLite round-trip tests, since fabricating a *sql.Rows from a
// fake executor isn't practical.)
//
// This is driver-independent: it inspects the args recorded by the
// fake executor rather than relying on a real driver round-trip,
// which could mask a missed normalization by re-parsing the value.
func TestBindArgNormalization_ExecEntryPoints(t *testing.T) {
	ctx := context.Background()
	stamp := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	want := stamp.UTC().Format(time.RFC3339Nano)

	type Row struct {
		ID        int64
		CreatedAt time.Time
	}
	type Plain struct {
		ID   int64
		Name string
	}

	t.Run("Insert row value", func(t *testing.T) {
		exec := &fakeExecutor{}
		if _, err := Insert(ctx, exec, "t", Row{CreatedAt: stamp}); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
	t.Run("InsertMany row value", func(t *testing.T) {
		exec := &fakeExecutor{}
		if err := InsertMany(ctx, exec, "t", []Row{{CreatedAt: stamp}}); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
	t.Run("Update SET value", func(t *testing.T) {
		exec := &fakeExecutor{}
		if _, err := Update(ctx, exec, "t", "id = ?", Row{CreatedAt: stamp}, 1); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
	t.Run("Update where arg", func(t *testing.T) {
		exec := &fakeExecutor{}
		if _, err := Update(ctx, exec, "t", "created_at = ?", Plain{Name: "x"}, stamp); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
	t.Run("Delete where arg", func(t *testing.T) {
		exec := &fakeExecutor{}
		if _, err := Delete(ctx, exec, "t", "created_at < ?", stamp); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
}

// assertNormalized fails if any bind arg is still a raw time.Time, or
// if the expected RFC3339Nano string is absent — i.e. normalization
// either didn't run or produced the wrong format.
func assertNormalized(t *testing.T, args []any, want string) {
	t.Helper()
	found := false
	for _, a := range args {
		if _, isTime := a.(time.Time); isTime {
			t.Fatalf("un-normalized time.Time reached the driver in args %v", args)
		}
		if s, ok := a.(string); ok && s == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("normalized time %q not found in bind args %v", want, args)
	}
}

// TestClassifierRegistry_ConcurrentRegisterAndClassify exercises
// the classifier registry under concurrent RegisterClassifier and
// in-flight classify. Run under `go test -race` to verify no data
// race surfaces.
//
// Note: the RWMutex already serializes correctly (writer's Lock
// excludes reader's RLock), and the existing append-only slice
// semantics mean the reader's snapshot of the slice header is
// internally consistent. The explicit `append([]Classifier(nil),
// classifiers...)` copy in classify is defensive — it documents
// that the iteration observes a non-aliased snapshot — but is not
// strictly required for correctness with the current RWMutex usage.
//
// Both goroutines do a bounded amount of work. An earlier version
// let the registrar append in an unbounded loop until the consumer
// finished; classify's O(n) snapshot copy over an ever-growing
// slice then turned the test into an O(n²) blowup that timed out
// (worse under -race). A fixed iteration count keeps it a genuine
// concurrency test that still terminates promptly.
func TestClassifierRegistry_ConcurrentRegisterAndClassify(t *testing.T) {
	saveClassifiers(t)

	const iterations = 2000

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			RegisterClassifier(func(error) error { return nil })
		}
	}()

	go func() {
		defer wg.Done()
		exec := &erroringExecutor{err: errors.New("driver: boom")}
		type Row struct {
			ID int64
			X  string
		}
		for i := 0; i < iterations; i++ {
			_, _ = Insert(context.Background(), exec, "t", Row{X: "x"})
		}
	}()

	wg.Wait()
}

// saveClassifiers isolates the global classifier registry for a test
// and restores it on cleanup, so registry-mutating tests don't leak
// into each other.
func saveClassifiers(t *testing.T) {
	t.Helper()
	classifiersMu.Lock()
	saved := append([]Classifier(nil), classifiers...)
	classifiers = nil
	classifiersMu.Unlock()
	t.Cleanup(func() {
		classifiersMu.Lock()
		classifiers = saved
		classifiersMu.Unlock()
	})
}

// TestClassifier_RegisteredTwiceRunsTwice locks in the documented
// "no deduplication" contract (errors.go): the same classifier
// registered twice is invoked twice per classify.
func TestClassifier_RegisteredTwiceRunsTwice(t *testing.T) {
	saveClassifiers(t)

	var calls int
	c := func(error) error { calls++; return nil } // never matches → all run
	RegisterClassifier(c)
	RegisterClassifier(c)

	_ = classify(errors.New("boom"))
	if calls != 2 {
		t.Errorf("classifier ran %d times, want 2 (the library does not deduplicate)", calls)
	}
}

// TestClassifierRegistry_SentinelReturnedUnderConcurrentRegister
// proves that concurrent RegisterClassifier calls don't disturb
// classify's correctness — the snapshot still includes the
// sentinel-returning classifier and every classify wraps ErrUnique.
// (The sibling test above only proves race-freedom.)
func TestClassifierRegistry_SentinelReturnedUnderConcurrentRegister(t *testing.T) {
	saveClassifiers(t)

	marker := errors.New("driver: boom")
	RegisterClassifier(func(err error) error {
		if errors.Is(err, marker) {
			return ErrUnique
		}
		return nil
	})

	const iterations = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			RegisterClassifier(func(error) error { return nil })
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if got := classify(marker); !errors.Is(got, ErrUnique) {
				t.Errorf("classify returned %v, want it to wrap ErrUnique", got)
				return
			}
		}
	}()

	wg.Wait()
}

// erroringExecutor always fails ExecContext with the configured
// error so every Insert call routes through classify.
type erroringExecutor struct{ err error }

func (e *erroringExecutor) ExecContext(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	return nil, e.err
}
func (e *erroringExecutor) QueryContext(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, e.err
}
func (e *erroringExecutor) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	return nil
}

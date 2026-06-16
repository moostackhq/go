package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moostackhq/go/sqlx"
	_ "github.com/moostackhq/go/sqlx/sqlite" // register the SQLite classifier
	_ "modernc.org/sqlite"                   // SQL driver
)

// open returns a fresh in-memory SQLite database. Tests get a clean
// slate each time, so isolation is per-test.
//
// SetMaxOpenConns(1) because SQLite `:memory:` databases are scoped
// to a single connection — without the cap, the connection pool can
// pick a different (empty) database for the next query.
func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// schema is the single canonical schema every test boots over.
const schema = `
CREATE TABLE users (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    email       TEXT    NOT NULL UNIQUE,
    name        TEXT,
    metadata    TEXT,
    created_at  TEXT    NOT NULL
);
CREATE TABLE posts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id),
    title       TEXT    NOT NULL,
    published   INTEGER NOT NULL
);
`

type User struct {
	ID        int64
	Email     string
	Name      sql.Null[string]
	Metadata  sqlx.JSON[map[string]any]
	CreatedAt time.Time
}

type Post struct {
	ID        int64
	UserID    int64
	Title     string
	Published bool
}

func setup(t *testing.T) *sql.DB {
	t.Helper()
	db := open(t)
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

// =====================================================================
// 1. Insert + QueryOne + auto-PK.
// =====================================================================

func TestInsert_AutoAssignsID(t *testing.T) {
	ctx := context.Background()
	db := setup(t)

	u := User{
		Email:     "a@x",
		Name:      sql.Null[string]{V: "Alice", Valid: true},
		Metadata:  sqlx.JSON[map[string]any]{V: map[string]any{"plan": "pro"}},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	id, err := sqlx.Insert(ctx, db, "users", u)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected non-zero auto-assigned id")
	}

	got, err := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "a@x" {
		t.Errorf("Email = %q, want a@x", got.Email)
	}
	if got.Name.V != "Alice" || !got.Name.Valid {
		t.Errorf("Name = %+v, want {Alice true}", got.Name)
	}
	if got.Metadata.V["plan"] != "pro" {
		t.Errorf("Metadata = %+v, want plan=pro", got.Metadata.V)
	}
}

func TestInsert_NonZeroIDPreserved(t *testing.T) {
	ctx := context.Background()
	db := setup(t)

	id, err := sqlx.Insert(ctx, db, "users", User{
		ID:        42,
		Email:     "explicit@x",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("returned id = %d, want 42 (caller's id should win)", id)
	}
	got, _ := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", 42)
	if got.ID != 42 {
		t.Errorf("persisted id = %d, want 42", got.ID)
	}
}

// TestInsert_PKOnlyZero_DefaultValues verifies issue #10 end-to-end:
// a struct whose only field is a zero-value auto-PK inserts via
// "INSERT INTO t DEFAULT VALUES" with the id auto-assigned. The
// "() VALUES ()" form the old code produced is accepted by SQLite but
// rejected by PostgreSQL, so DEFAULT VALUES is the portable choice.
func TestInsert_PKOnlyZero_DefaultValues(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE counters (id INTEGER PRIMARY KEY AUTOINCREMENT)`); err != nil {
		t.Fatal(err)
	}

	type Counter struct{ ID int64 }
	id1, err := sqlx.Insert(ctx, db, "counters", Counter{})
	if err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	id2, err := sqlx.Insert(ctx, db, "counters", Counter{})
	if err != nil {
		t.Fatalf("second Insert: %v", err)
	}
	if id1 == 0 || id2 == 0 || id1 == id2 {
		t.Fatalf("expected two distinct non-zero ids, got %d and %d", id1, id2)
	}

	rows, err := sqlx.Query[Counter](ctx, db, "SELECT id FROM counters ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2", len(rows))
	}
}

// =====================================================================
// 2. QueryOne returns ErrNotFound.
// =====================================================================

func TestQueryOne_NotFound(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	_, err := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", 999)
	if !errors.Is(err, sqlx.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// =====================================================================
// 3. Query (many rows) + ordering preserved + snake_case mapping.
// =====================================================================

func TestQuery_MultipleRows(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i, email := range []string{"a@x", "b@x", "c@x"} {
		if _, err := sqlx.Insert(ctx, db, "users", User{
			Email: email, CreatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := sqlx.Query[User](ctx, db, "SELECT * FROM users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	for i, want := range []string{"a@x", "b@x", "c@x"} {
		if got[i].Email != want {
			t.Errorf("row[%d].Email = %q, want %q", i, got[i].Email, want)
		}
	}
}

// TestQuery_NonStructT_Errors covers the read-path contract that T
// must be a struct. A concrete scalar (int) and an interface (any)
// must both return a clear error rather than panic — the interface
// case previously nil-deref'd in scanRow because reflect.TypeOf of a
// nil interface is nil. A row must exist so scanRow is actually
// reached.
func TestQuery_NonStructT_Errors(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	if _, err := sqlx.Insert(ctx, db, "users", User{Email: "a@x", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlx.Query[int](ctx, db, "SELECT id FROM users"); err == nil || !strings.Contains(err.Error(), "must be a struct") {
		t.Errorf("Query[int] err = %v, want a 'must be a struct' error", err)
	}
	if _, err := sqlx.Query[any](ctx, db, "SELECT id FROM users"); err == nil || !strings.Contains(err.Error(), "must be a struct") {
		t.Errorf("Query[any] err = %v, want a 'must be a struct' error (no panic)", err)
	}
	if _, err := sqlx.QueryOne[any](ctx, db, "SELECT id FROM users"); err == nil || !strings.Contains(err.Error(), "must be a struct") {
		t.Errorf("QueryOne[any] err = %v, want a 'must be a struct' error (no panic)", err)
	}
}

// TestQuery_ColumnMatchIsCaseInsensitive verifies that result columns
// whose case differs from the struct's snake_case field names still
// map correctly. Aliases force mixed-case result-column names; without
// case-insensitive matching they would silently scan into a discard
// and leave the fields zero.
func TestQuery_ColumnMatchIsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	stamp := time.Now().UTC().Truncate(time.Second)
	if _, err := sqlx.Insert(ctx, db, "users", User{Email: "a@x", CreatedAt: stamp}); err != nil {
		t.Fatal(err)
	}

	got, err := sqlx.QueryOne[User](ctx, db,
		`SELECT id AS ID, email AS Email, name AS Name, metadata AS Metadata, created_at AS CREATED_AT FROM users`)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 {
		t.Errorf("ID not scanned from aliased column 'ID'")
	}
	if got.Email != "a@x" {
		t.Errorf("Email = %q, want a@x (mixed-case result column must match)", got.Email)
	}
	if !got.CreatedAt.Equal(stamp) {
		t.Errorf("CreatedAt = %v, want %v (matched from 'CREATED_AT')", got.CreatedAt, stamp)
	}
}

// TestQuery_CaseOnlyDuplicateColumn_Errors confirms the ambiguity
// guard is also case-insensitive: "id" and "ID" both match the same
// field, so the result is rejected rather than silently scanned twice.
func TestQuery_CaseOnlyDuplicateColumn_Errors(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	if _, err := sqlx.Insert(ctx, db, "users", User{Email: "a@x", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	type Row struct{ ID int64 }
	_, err := sqlx.Query[Row](ctx, db, "SELECT id, id AS ID FROM users")
	if err == nil || !strings.Contains(err.Error(), "appears at positions") {
		t.Errorf("err = %v, want a duplicate-column error for the id/ID case collision", err)
	}
}

// TestQuotedDBTag_RoundTrip verifies a double-quoted db: tag (used to
// name a reserved-word / mixed-case column) survives a full write +
// read. The driver reports the column unquoted ("Order"), so the
// colToField key must be built from the unquoted name; otherwise the
// scan lookup misses and the field silently reads back as zero.
func TestQuotedDBTag_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE things (id INTEGER PRIMARY KEY AUTOINCREMENT, "Order" INTEGER)`); err != nil {
		t.Fatal(err)
	}

	type Thing struct {
		ID    int64
		Order int64 `db:"\"Order\""`
	}
	id, err := sqlx.Insert(ctx, db, "things", Thing{Order: 42})
	if err != nil {
		t.Fatalf("insert with quoted db tag: %v", err)
	}
	got, err := sqlx.QueryOne[Thing](ctx, db, "SELECT * FROM things WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Order != 42 {
		t.Errorf("Order = %d, want 42 (quoted-column read must not silently scan as zero)", got.Order)
	}
}

// =====================================================================
// 4. InsertMany.
// =====================================================================

func TestInsertMany(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	// One parent for the FK.
	uid, _ := sqlx.Insert(ctx, db, "users", User{Email: "p@x", CreatedAt: time.Now().UTC()})

	posts := []Post{
		{UserID: uid, Title: "p1", Published: true},
		{UserID: uid, Title: "p2", Published: false},
		{UserID: uid, Title: "p3", Published: true},
	}
	if err := sqlx.InsertMany(ctx, db, "posts", posts); err != nil {
		t.Fatal(err)
	}
	got, _ := sqlx.Query[Post](ctx, db, "SELECT * FROM posts ORDER BY id")
	if len(got) != 3 {
		t.Errorf("rows after InsertMany = %d, want 3", len(got))
	}
	if got[0].Title != "p1" || got[2].Title != "p3" {
		t.Errorf("titles out of order: %+v", got)
	}
}

func TestInsertMany_EmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	if err := sqlx.InsertMany(ctx, db, "users", []User{}); err != nil {
		t.Errorf("InsertMany([]) = %v, want nil", err)
	}
}

// TestInsertMany_PKIncludedWritesZeroAsZero locks in the documented
// auto-PK rule: if any row has a non-zero id, the id column is
// included for the whole batch, and a row whose id is zero is written
// verbatim as 0 — NOT auto-assigned per row.
func TestInsertMany_PKIncludedWritesZeroAsZero(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := sqlx.InsertMany(ctx, db, "users", []User{
		{ID: 100, Email: "hundred@x", CreatedAt: now},
		{ID: 0, Email: "zero@x", CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	zero, err := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE email = ?", "zero@x")
	if err != nil {
		t.Fatal(err)
	}
	if zero.ID != 0 {
		t.Errorf("zero-id row got id=%d, want 0 (PK included → zero written verbatim, not auto-assigned)", zero.ID)
	}
	hundred, err := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE email = ?", "hundred@x")
	if err != nil {
		t.Fatal(err)
	}
	if hundred.ID != 100 {
		t.Errorf("explicit-id row got id=%d, want 100", hundred.ID)
	}
}

// =====================================================================
// 5. Update — sets every field in the struct.
// =====================================================================

type UserEmailUpdate struct {
	Email string
	Name  sql.Null[string]
}

func TestUpdate_PartialViaSmallerStruct(t *testing.T) {
	ctx := context.Background()
	db := setup(t)

	id, _ := sqlx.Insert(ctx, db, "users", User{
		Email: "old@x", CreatedAt: time.Now().UTC(),
	})

	upd := UserEmailUpdate{
		Email: "new@x",
		Name:  sql.Null[string]{V: "Bob", Valid: true},
	}
	n, err := sqlx.Update(ctx, db, "users", "id = ?", upd, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Update affected %d rows, want 1", n)
	}
	got, _ := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", id)
	if got.Email != "new@x" || got.Name.V != "Bob" {
		t.Errorf("after update: Email=%q Name=%+v", got.Email, got.Name)
	}
}

// =====================================================================
// 6. Delete.
// =====================================================================

func TestDelete(t *testing.T) {
	ctx := context.Background()
	db := setup(t)

	id, _ := sqlx.Insert(ctx, db, "users", User{Email: "d@x", CreatedAt: time.Now().UTC()})
	n, err := sqlx.Delete(ctx, db, "users", "id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Delete affected %d rows, want 1", n)
	}
	if _, err := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", id); !errors.Is(err, sqlx.ErrNotFound) {
		t.Errorf("after delete: err = %v, want ErrNotFound", err)
	}
	// A second delete affects nothing — the count, not an error, is
	// how callers detect "no such row".
	if n, err := sqlx.Delete(ctx, db, "users", "id = ?", id); err != nil || n != 0 {
		t.Errorf("re-delete: n=%d err=%v, want 0, nil", n, err)
	}
}

// =====================================================================
// 7. Tx commit.
// =====================================================================

func TestTx_CommitsOnNilReturn(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	err := sqlx.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := sqlx.Insert(ctx, tx, "users", User{Email: "tx@x", CreatedAt: time.Now().UTC()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := sqlx.Query[User](ctx, db, "SELECT * FROM users")
	if len(got) != 1 {
		t.Errorf("after commit: %d rows, want 1", len(got))
	}
}

// =====================================================================
// 8. Tx rollback on error.
// =====================================================================

func TestTx_RollsBackOnError(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	wantErr := errors.New("planned failure")
	err := sqlx.Tx(ctx, db, func(tx *sql.Tx) error {
		if _, err := sqlx.Insert(ctx, tx, "users", User{Email: "rolled@x", CreatedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wraps planned failure", err)
	}
	got, _ := sqlx.Query[User](ctx, db, "SELECT * FROM users")
	if len(got) != 0 {
		t.Errorf("rollback failed: %d rows present, want 0", len(got))
	}
}

// TestTx_RollbackFailureWrapsBothErrors covers the path where fn
// returns an error AND the library's own Rollback then fails: Tx must
// return an error that still wraps fn's error (so errors.Is works)
// and also names the rollback failure. We force the Rollback to fail
// by finishing the transaction inside fn — the deferred/secondary
// Rollback in Tx then returns sql.ErrTxDone.
func TestTx_RollbackFailureWrapsBothErrors(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	sentinel := errors.New("business failure")

	err := sqlx.Tx(ctx, db, func(tx *sql.Tx) error {
		if rerr := tx.Rollback(); rerr != nil { // end the tx ourselves
			t.Fatalf("manual rollback: %v", rerr)
		}
		return sentinel // Tx's own Rollback now returns sql.ErrTxDone
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the fn error", err)
	}
	if !strings.Contains(err.Error(), "rollback:") {
		t.Errorf("err = %q, want it to also report the rollback failure", err)
	}
}

// =====================================================================
// 9. Tx rollback on panic.
// =====================================================================

func TestTx_RollsBackOnPanic(t *testing.T) {
	ctx := context.Background()
	db := setup(t)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = sqlx.Tx(ctx, db, func(tx *sql.Tx) error {
			_, _ = sqlx.Insert(ctx, tx, "users", User{Email: "boom@x", CreatedAt: time.Now().UTC()})
			panic("boom")
		})
	}()

	got, _ := sqlx.Query[User](ctx, db, "SELECT * FROM users")
	if len(got) != 0 {
		t.Errorf("panic rollback failed: %d rows, want 0", len(got))
	}
}

// =====================================================================
// 10. Error classification: unique violation.
// =====================================================================

func TestErr_UniqueViolation(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	now := time.Now().UTC()
	if _, err := sqlx.Insert(ctx, db, "users", User{Email: "dup@x", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	_, err := sqlx.Insert(ctx, db, "users", User{Email: "dup@x", CreatedAt: now})
	if !errors.Is(err, sqlx.ErrUnique) {
		t.Errorf("err = %v, want errors.Is(...ErrUnique) true", err)
	}
}

// =====================================================================
// 11. Error classification: foreign key violation.
// =====================================================================

func TestErr_ForeignKeyViolation(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	_, err := sqlx.Insert(ctx, db, "posts", Post{UserID: 999, Title: "orphan", Published: false})
	if !errors.Is(err, sqlx.ErrForeignKey) {
		t.Errorf("err = %v, want errors.Is(...ErrForeignKey) true", err)
	}
}

// =====================================================================
// 12. JSON[T] round-trip.
// =====================================================================

func TestJSON_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := setup(t)

	meta := map[string]any{
		"plan":     "pro",
		"flags":    []any{"beta", "alpha"},
		"quota_mb": float64(1024),
	}
	id, _ := sqlx.Insert(ctx, db, "users", User{
		Email:     "j@x",
		Metadata:  sqlx.JSON[map[string]any]{V: meta},
		CreatedAt: time.Now().UTC(),
	})
	got, _ := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", id)
	if got.Metadata.V["plan"] != "pro" {
		t.Errorf("metadata.plan = %v, want pro", got.Metadata.V["plan"])
	}
	if flags, _ := got.Metadata.V["flags"].([]any); len(flags) != 2 {
		t.Errorf("metadata.flags = %v, want 2 entries", flags)
	}
}

// TestJSON_NilPointerPayload covers JSON[*T] whose payload pointer is
// nil: it must marshal to JSON "null" on insert and unmarshal back to
// a nil pointer on scan — not panic and not error. A non-nil pointer
// round-trips its value, confirming the column itself works.
func TestJSON_NilPointerPayload(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE docs (id INTEGER PRIMARY KEY AUTOINCREMENT, data TEXT)`); err != nil {
		t.Fatal(err)
	}

	type Profile struct{ Bio string }
	type Doc struct {
		ID   int64
		Data sqlx.JSON[*Profile]
	}

	id, err := sqlx.Insert(ctx, db, "docs", Doc{Data: sqlx.JSON[*Profile]{V: nil}})
	if err != nil {
		t.Fatalf("insert nil-pointer payload: %v", err)
	}
	got, err := sqlx.QueryOne[Doc](ctx, db, "SELECT * FROM docs WHERE id = ?", id)
	if err != nil {
		t.Fatalf("scan nil-pointer payload: %v", err)
	}
	if got.Data.V != nil {
		t.Errorf("nil-pointer payload round-tripped to %+v, want nil", got.Data.V)
	}

	id2, err := sqlx.Insert(ctx, db, "docs", Doc{Data: sqlx.JSON[*Profile]{V: &Profile{Bio: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	got2, err := sqlx.QueryOne[Doc](ctx, db, "SELECT * FROM docs WHERE id = ?", id2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Data.V == nil || got2.Data.V.Bio != "hi" {
		t.Errorf("non-nil pointer payload = %+v, want Bio=hi", got2.Data.V)
	}
}

// TestJSON_InsertMarshalErrorSurfaces confirms that a JSON payload
// that cannot be marshaled (a chan) surfaces as an error from Insert
// — database/sql calls Value() at the driver boundary — rather than
// panicking.
func TestJSON_InsertMarshalErrorSurfaces(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE docs (id INTEGER PRIMARY KEY AUTOINCREMENT, data TEXT)`); err != nil {
		t.Fatal(err)
	}
	type Doc struct {
		ID   int64
		Data sqlx.JSON[chan int]
	}
	if _, err := sqlx.Insert(ctx, db, "docs", Doc{Data: sqlx.JSON[chan int]{V: make(chan int)}}); err == nil {
		t.Fatal("Insert with unmarshalable JSON payload = nil error, want an error (no panic)")
	}
}

// =====================================================================
// 13. sql.Null[T] writes NULL on !Valid, scans NULL into zero.
// =====================================================================

// =====================================================================
// Bind-arg normalization for time.Time across every entry point.
// Regression for the bug where Insert normalized time.Time to
// RFC3339Nano but Query / Update / Delete passed the bind value
// through unchanged, so WHERE clauses comparing against a time.Time
// argument used a different string format than the stored row.
// =====================================================================

func TestQuery_TimeBindArgNormalized(t *testing.T) {
	ctx := context.Background()
	db := setup(t)

	early := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	veryLate := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := sqlx.Insert(ctx, db, "users", User{Email: "a@x", CreatedAt: late}); err != nil {
		t.Fatal(err)
	}

	got, err := sqlx.Query[User](ctx, db, "SELECT * FROM users WHERE created_at > ?", early)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("WHERE created_at > earlier: got %d rows, want 1 (time bind args must normalize to match stored format)", len(got))
	}

	got, err = sqlx.Query[User](ctx, db, "SELECT * FROM users WHERE created_at > ?", veryLate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("WHERE created_at > later: got %d rows, want 0", len(got))
	}
}

func TestQueryOne_TimeBindArgNormalized(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	stamp := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if _, err := sqlx.Insert(ctx, db, "users", User{Email: "a@x", CreatedAt: stamp}); err != nil {
		t.Fatal(err)
	}
	got, err := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE created_at = ?", stamp)
	if err != nil {
		t.Fatalf("QueryOne WHERE created_at = same stamp: %v (bind format must match stored format)", err)
	}
	if got.Email != "a@x" {
		t.Errorf("matched wrong row: %+v", got)
	}
}

func TestUpdate_WhereArgsTimeNormalized(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	stamp := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	id, _ := sqlx.Insert(ctx, db, "users", User{Email: "a@x", CreatedAt: stamp})

	upd := UserEmailUpdate{Email: "new@x"}
	if _, err := sqlx.Update(ctx, db, "users", "created_at = ?", upd, stamp); err != nil {
		t.Fatal(err)
	}
	got, _ := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", id)
	if got.Email != "new@x" {
		t.Errorf("UPDATE … WHERE created_at = ? did not match (got %q); whereArgs must normalize", got.Email)
	}
}

func TestDelete_WhereArgsTimeNormalized(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	earlier := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := sqlx.Insert(ctx, db, "users", User{Email: "old@x", CreatedAt: earlier}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlx.Insert(ctx, db, "users", User{Email: "new@x", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlx.Delete(ctx, db, "users", "created_at < ?", cutoff); err != nil {
		t.Fatal(err)
	}
	got, _ := sqlx.Query[User](ctx, db, "SELECT * FROM users")
	if len(got) != 1 || got[0].Email != "new@x" {
		t.Errorf("after DELETE … WHERE created_at < ?: rows = %+v, want only new@x", got)
	}
}

// =====================================================================
// JOINs with colliding result column names must fail loudly. A user
// who writes "SELECT users.id, posts.id …" gets two columns named
// "id" in rows.Columns(); without the check, the scanner silently
// reads one column twice into whichever struct field maps to "id".
// =====================================================================

func TestQuery_JoinWithCollidingColumns_Errors(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	uid, _ := sqlx.Insert(ctx, db, "users", User{Email: "a@x", CreatedAt: time.Now().UTC()})
	if err := sqlx.InsertMany(ctx, db, "posts", []Post{{UserID: uid, Title: "t1"}}); err != nil {
		t.Fatal(err)
	}

	type UserPost struct {
		ID    int64
		Email string
		Title string
	}
	_, err := sqlx.Query[UserPost](ctx, db, `
		SELECT users.id, users.email, posts.id, posts.title
		FROM users JOIN posts ON posts.user_id = users.id`)
	if err == nil {
		t.Fatal("expected error from JOIN with colliding columns; got nil")
	}
	if !strings.Contains(err.Error(), `column "id" appears`) {
		t.Errorf("error message %q does not name the duplicate column", err.Error())
	}
	if !strings.Contains(err.Error(), "alias one") {
		t.Errorf("error message %q does not hint at the fix", err.Error())
	}
}

func TestQuery_JoinWithAliasedColumns_Succeeds(t *testing.T) {
	ctx := context.Background()
	db := setup(t)
	uid, _ := sqlx.Insert(ctx, db, "users", User{Email: "a@x", CreatedAt: time.Now().UTC()})
	if err := sqlx.InsertMany(ctx, db, "posts", []Post{{UserID: uid, Title: "t1"}}); err != nil {
		t.Fatal(err)
	}

	type UserPost struct {
		UserID    int64  `db:"user_id"`
		UserEmail string `db:"user_email"`
		PostID    int64  `db:"post_id"`
		PostTitle string `db:"post_title"`
	}
	got, err := sqlx.Query[UserPost](ctx, db, `
		SELECT users.id    AS user_id,
		       users.email AS user_email,
		       posts.id    AS post_id,
		       posts.title AS post_title
		FROM users JOIN posts ON posts.user_id = users.id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].UserEmail != "a@x" || got[0].PostTitle != "t1" {
		t.Errorf("got %+v, want UserEmail=a@x PostTitle=t1", got[0])
	}
}

// =====================================================================
// *time.Time round-trip. Nullable time columns round-trip with
// `*time.Time` struct fields: nil writes NULL, NULL reads as nil,
// a value writes RFC3339Nano TEXT and reads back to a fresh pointer.
// =====================================================================

func setupEvents(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE events (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		title        TEXT    NOT NULL,
		scheduled_at TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	return db, func() {}
}

type Event struct {
	ID          int64
	Title       string
	ScheduledAt *time.Time
}

func TestPtrTime_NilWritesAndReadsAsNULL(t *testing.T) {
	ctx := context.Background()
	db, _ := setupEvents(t)

	id, err := sqlx.Insert(ctx, db, "events", Event{Title: "draft", ScheduledAt: nil})
	if err != nil {
		t.Fatal(err)
	}

	// Verify the column persisted as NULL, not as "" or some pointer string.
	var raw sql.NullString
	if err := db.QueryRow(`SELECT scheduled_at FROM events WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Valid {
		t.Errorf("scheduled_at persisted as %q (Valid=true); want NULL", raw.String)
	}

	// Scan back into Event{ScheduledAt *time.Time}: should be nil.
	got, err := sqlx.QueryOne[Event](ctx, db, "SELECT * FROM events WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScheduledAt != nil {
		t.Errorf("scan: ScheduledAt = %v, want nil", got.ScheduledAt)
	}
}

func TestPtrTime_ValueWritesAndReadsAsTimestamp(t *testing.T) {
	ctx := context.Background()
	db, _ := setupEvents(t)

	want := time.Date(2026, 7, 4, 15, 30, 0, 0, time.UTC)
	id, err := sqlx.Insert(ctx, db, "events", Event{Title: "launch", ScheduledAt: &want})
	if err != nil {
		t.Fatal(err)
	}

	// Stored as RFC3339Nano TEXT, not as a Go pointer string.
	var raw string
	if err := db.QueryRow(`SELECT scheduled_at FROM events WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != want.Format(time.RFC3339Nano) {
		t.Errorf("stored TEXT = %q, want %q", raw, want.Format(time.RFC3339Nano))
	}

	got, err := sqlx.QueryOne[Event](ctx, db, "SELECT * FROM events WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScheduledAt == nil {
		t.Fatal("scan: ScheduledAt is nil; want a pointer to the stored value")
	}
	if !got.ScheduledAt.Equal(want) {
		t.Errorf("scan: ScheduledAt = %v, want %v", *got.ScheduledAt, want)
	}
}

func TestPtrTime_WhereBindArgNormalized(t *testing.T) {
	ctx := context.Background()
	db, _ := setupEvents(t)

	t1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := sqlx.Insert(ctx, db, "events", Event{Title: "june", ScheduledAt: &t1}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlx.Insert(ctx, db, "events", Event{Title: "july", ScheduledAt: &t2}); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	got, err := sqlx.Query[Event](ctx, db,
		`SELECT * FROM events WHERE scheduled_at > ? ORDER BY id`, &cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "july" {
		t.Errorf("rows after WHERE scheduled_at > cutoff (pointer) = %+v, want only july", got)
	}
}

func TestNull_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := setup(t)

	id, _ := sqlx.Insert(ctx, db, "users", User{
		Email:     "nil@x",
		Name:      sql.Null[string]{}, // !Valid → write NULL
		CreatedAt: time.Now().UTC(),
	})

	var raw sql.NullString
	if err := db.QueryRow("SELECT name FROM users WHERE id = ?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Valid {
		t.Errorf("column persisted as %q (Valid=true); want NULL", raw.String)
	}

	got, _ := sqlx.QueryOne[User](ctx, db, "SELECT * FROM users WHERE id = ?", id)
	if got.Name.Valid {
		t.Errorf("scan back: Name=%+v, want zero Null{}", got.Name)
	}
}

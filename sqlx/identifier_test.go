package sqlx

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestValidIdentifier(t *testing.T) {
	valid := []string{
		"users",
		"_users",
		"Users",
		"user_accounts",
		"t1",
		"col$",
		"public.users",
		"my_schema.user_accounts",
		`"order"`,             // reserved word, quoted
		`public."weird name"`, // quoted segment with a space
		`"a""b"`,              // "" escapes an embedded quote
	}
	for _, s := range valid {
		if !validIdentifier(s) {
			t.Errorf("validIdentifier(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",
		"1users",              // leading digit
		"users; DROP TABLE x", // statement injection
		"users WHERE 1=1",     // trailing SQL
		"users--",             // comment
		"user accounts",       // bare space
		"users(",              // paren
		"users,posts",         // comma
		".users",              // leading dot
		"users.",              // trailing dot
		"public..users",       // empty segment
		`"unterminated`,       // unbalanced quote
		`"users"; DROP`,       // quoted then injection
		"public.users; DROP",  // qualified then injection
	}
	for _, s := range invalid {
		if validIdentifier(s) {
			t.Errorf("validIdentifier(%q) = true, want false", s)
		}
	}
}

func TestUnquoteIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"order", "order"},               // bare → unchanged
		{`"Order"`, "Order"},             // quoted → quotes stripped
		{`"a""b"`, `a"b`},                // "" escape collapsed
		{`""`, ""},                       // empty quoted
		{"public.users", "public.users"}, // dotted bare → unchanged
		{"", ""},                         // empty
	}
	for _, c := range cases {
		if got := unquoteIdent(c.in); got != c.want {
			t.Errorf("unquoteIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

type idRow struct {
	ID   int64
	Name string
}

// TestWriteHelpers_RejectMaliciousTable asserts that every write
// helper validates the table argument before touching the database,
// returning ErrInvalidIdentifier and issuing no query.
func TestWriteHelpers_RejectMaliciousTable(t *testing.T) {
	const evil = "users; DROP TABLE users; --"

	t.Run("Insert", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Insert(context.Background(), exec, evil, idRow{Name: "x"})
		assertRejected(t, exec, err)
	})
	t.Run("InsertMany", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, evil, []idRow{{Name: "x"}})
		assertRejected(t, exec, err)
	})
	t.Run("Update", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, evil, "id = ?", idRow{Name: "x"}, 1)
		assertRejected(t, exec, err)
	})
	t.Run("Delete", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Delete(context.Background(), exec, evil, "id = ?", 1)
		assertRejected(t, exec, err)
	})
}

// TestWriteHelpers_RejectEmptyTable asserts the empty string is not a
// valid table identifier: each helper returns ErrInvalidIdentifier
// and issues no query, rather than emitting "INSERT INTO  (...)".
func TestWriteHelpers_RejectEmptyTable(t *testing.T) {
	t.Run("Insert", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Insert(context.Background(), exec, "", idRow{Name: "x"})
		assertRejected(t, exec, err)
	})
	t.Run("InsertMany", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, "", []idRow{{Name: "x"}})
		assertRejected(t, exec, err)
	})
	t.Run("Update", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, "", "id = ?", idRow{Name: "x"}, 1)
		assertRejected(t, exec, err)
	})
	t.Run("Delete", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Delete(context.Background(), exec, "", "id = ?", 1)
		assertRejected(t, exec, err)
	})
}

func assertRejected(t *testing.T, exec *fakeExecutor, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("error = %v, want ErrInvalidIdentifier", err)
	}
	if len(exec.queries) != 0 {
		t.Fatalf("executed %d queries, want 0: %q", len(exec.queries), exec.queries)
	}
}

// TestWriteHelpers_AllowValidTable confirms legitimate identifiers —
// including schema-qualified and quoted forms — are not rejected.
func TestWriteHelpers_AllowValidTable(t *testing.T) {
	for _, table := range []string{"users", "public.users", `"order"`} {
		exec := &fakeExecutor{}
		if _, err := Insert(context.Background(), exec, table, idRow{Name: "x"}); err != nil {
			t.Errorf("Insert into %q: unexpected error %v", table, err)
		}
		if len(exec.queries) != 1 || !strings.Contains(exec.queries[0], table) {
			t.Errorf("Insert into %q: query = %q", table, exec.queries)
		}
	}
}

// TestBuildFieldMap_ErrorsOnInvalidColumn verifies a db: tag that is
// not a valid identifier is caught at field-map build time and
// surfaced as ErrInvalidIdentifier rather than interpolated into a
// statement.
func TestBuildFieldMap_ErrorsOnInvalidColumn(t *testing.T) {
	type Bad struct {
		ID   int64
		Evil string `db:"name = '' OR 1=1; --"`
	}
	_, err := buildFieldMap(reflect.TypeOf(Bad{}))
	if !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("error = %v, want ErrInvalidIdentifier", err)
	}
	if !strings.Contains(err.Error(), "Evil") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

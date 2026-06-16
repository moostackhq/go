package sqlx

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// Insert writes row into table. Column names are derived from the
// row struct's fields (snake_case + db: tag override). The returned
// int64 is the row's primary key.
//
// Convention: a field whose column name is "id" is the auto-PK.
//   - When the field is the zero value, the column is omitted from
//     the INSERT so the database assigns the id; the return is
//     [sql.Result.LastInsertId] (0 on drivers without it, like pgx —
//     use raw INSERT … RETURNING id there).
//   - When the field is non-zero, the column is included verbatim
//     and the return is that supplied value, NOT LastInsertId. This
//     means an explicit PK round-trips identically on every driver,
//     even those that do not implement LastInsertId.
//
// For non-integer PKs (string UUIDs, etc.) the supplied value is the
// truth but cannot be expressed as int64 — the return is 0 in that
// case and the caller already knows the id.
func Insert(ctx context.Context, db Executor, table string, row any) (int64, error) {
	if err := checkTable("sqlx.Insert", table); err != nil {
		return 0, err
	}
	typ := reflect.TypeOf(row)
	// typ is nil when row is an untyped nil passed through any; guard
	// before Kind(), which would otherwise nil-deref.
	if typ == nil || typ.Kind() != reflect.Struct {
		return 0, fmt.Errorf("sqlx.Insert: row must be a struct, got %v", reflect.TypeOf(row))
	}
	fm, err := getFieldMap(typ)
	if err != nil {
		return 0, err
	}
	if err := checkColumns("sqlx.Insert", fm, typ); err != nil {
		return 0, err
	}
	val := reflect.ValueOf(row)

	cols, placeholders, args := buildInsertParts(fm, val)
	var query string
	if len(cols) == 0 {
		// Every column was omitted — the struct's only field is a
		// zero-value auto-PK, so the DB assigns everything. Use the
		// portable DEFAULT VALUES form; "INSERT INTO t () VALUES ()"
		// is accepted by SQLite but rejected by PostgreSQL.
		query = fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", table)
	} else {
		query = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
			table, strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	}
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, classify(err)
	}

	// Caller supplied a non-zero PK → return that. Otherwise the DB
	// assigned the id and we read it via LastInsertId.
	if fm.pkIndex >= 0 {
		pkVal := val.Field(fm.fields[fm.pkIndex].index)
		if !pkVal.IsZero() {
			return pkAsInt64(pkVal), nil
		}
	}
	id, _ := res.LastInsertId() // 0 on drivers without it (e.g. pgx)
	return id, nil
}

// checkColumns guards against a row struct that contributes no
// columns — an empty struct or one with only unexported fields. It
// returns an [ErrNoColumns]-wrapped error naming the calling function
// and the type, so the failure is reported clearly instead of as a
// malformed statement (e.g. UPDATE … SET  WHERE …) reaching the
// driver. fn is the qualified name used in the message, e.g.
// "sqlx.Update".
func checkColumns(fn string, fm *fieldMap, typ reflect.Type) error {
	if len(fm.fields) == 0 {
		return fmt.Errorf("%w: %s: type %s has no exported fields", ErrNoColumns, fn, typ)
	}
	return nil
}

// pkAsInt64 returns a non-zero PK reflect.Value as int64. Integer
// kinds round-trip exactly when representable; a uint64 larger than
// math.MaxInt64 has no faithful int64 form, so — like non-numeric
// kinds (string UUIDs, byte slices, etc.) — it returns 0. In every
// 0-return case the caller already holds the id it supplied.
func pkAsInt64(v reflect.Value) int64 {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := v.Uint()
		if u > math.MaxInt64 {
			// Beyond int64's range: no exact conversion (int64(u)
			// would wrap negative), so return 0 rather than a wrong id.
			return 0
		}
		return int64(u)
	default:
		return 0
	}
}

// InsertMany writes a slice of rows in a single INSERT … VALUES
// (…),(…),(…) statement. The slice element type must be a struct.
//
// All rows share the column list determined from the slice element
// type. The auto-PK rule from [Insert] applies row-by-row: if the
// id column is zero for every row, it is omitted from the column
// list; otherwise the column is included and individual rows' zero
// ids are written as 0 (NOT auto-assigned per row).
func InsertMany(ctx context.Context, db Executor, table string, rows any) error {
	if err := checkTable("sqlx.InsertMany", table); err != nil {
		return err
	}
	val := reflect.ValueOf(rows)
	if val.Kind() != reflect.Slice {
		return fmt.Errorf("sqlx.InsertMany: rows must be a slice, got %s", val.Kind())
	}
	if val.Len() == 0 {
		return nil
	}
	elemType := val.Type().Elem()
	if elemType.Kind() != reflect.Struct {
		return fmt.Errorf("sqlx.InsertMany: slice element must be a struct, got %s", elemType.Kind())
	}
	fm, err := getFieldMap(elemType)
	if err != nil {
		return err
	}
	if err := checkColumns("sqlx.InsertMany", fm, elemType); err != nil {
		return err
	}

	// Decide whether to include the auto-PK column: include only
	// if at least one row has a non-zero id.
	includePK := fm.pkIndex < 0
	if !includePK {
		for i := 0; i < val.Len(); i++ {
			if !val.Index(i).Field(fm.fields[fm.pkIndex].index).IsZero() {
				includePK = true
				break
			}
		}
	}

	cols := []string{}
	colFieldIdx := []int{} // fields[].index for each chosen column, in order
	for i, f := range fm.fields {
		if !includePK && i == fm.pkIndex {
			continue
		}
		cols = append(cols, f.column)
		colFieldIdx = append(colFieldIdx, f.index)
	}
	if len(cols) == 0 {
		// Every row would insert only an auto-assigned id (all PKs
		// zero, no other columns). DEFAULT VALUES inserts a single row
		// and there is no portable multi-row all-defaults form across
		// SQLite and PostgreSQL, so this is rejected rather than
		// guessed at — and it also avoids the negative Repeat below.
		// Supply at least one non-PK column, or call Insert per row.
		return fmt.Errorf("%w: sqlx.InsertMany: every row of %s would insert only an auto-assigned id — supply a non-PK column or use Insert per row",
			ErrNoColumns, elemType)
	}

	placeholders := "(" + strings.Repeat("?, ", len(cols)-1) + "?)"
	values := make([]string, val.Len())
	args := make([]any, 0, val.Len()*len(cols))
	for r := 0; r < val.Len(); r++ {
		rv := val.Index(r)
		for _, fi := range colFieldIdx {
			args = append(args, argOf(rv.Field(fi).Interface()))
		}
		values[r] = placeholders
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s",
		table, strings.Join(cols, ", "), strings.Join(values, ", "))
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		return classify(err)
	}
	return nil
}

// Update sets every column in row on rows matching the where
// expression and returns the number of rows affected. Args after the
// row are bound to the where placeholders in order.
//
//	n, err := sqlx.Update(ctx, db, "users", "id = ?",
//	    UserUpdate{Email: "x", Name: "y"}, userID)
//
// To do a partial update, define a smaller struct describing only
// the columns to change. The count lets callers apply their own
// policy — e.g. treat n == 0 as "not found" — without dropping to
// raw database/sql; it comes from [database/sql.Result.RowsAffected].
//
// Security: table is validated as a SQL identifier (returns
// [ErrInvalidIdentifier] otherwise). where is interpolated verbatim —
// it is a trusted SQL fragment, not user input. Bind every value
// through a ? placeholder and pass it in whereArgs; never concatenate
// user input into where.
//
// where is not validated and must be non-empty: passing "" emits a
// dangling WHERE and yields a cryptic driver error. To update every
// row deliberately, pass a constant true predicate such as "1 = 1".
func Update(ctx context.Context, db Executor, table, where string, row any, whereArgs ...any) (int64, error) {
	if err := checkTable("sqlx.Update", table); err != nil {
		return 0, err
	}
	typ := reflect.TypeOf(row)
	// typ is nil when row is an untyped nil passed through any; guard
	// before Kind(), which would otherwise nil-deref.
	if typ == nil || typ.Kind() != reflect.Struct {
		return 0, fmt.Errorf("sqlx.Update: row must be a struct, got %v", reflect.TypeOf(row))
	}
	fm, err := getFieldMap(typ)
	if err != nil {
		return 0, err
	}
	if err := checkColumns("sqlx.Update", fm, typ); err != nil {
		return 0, err
	}
	val := reflect.ValueOf(row)

	sets := make([]string, len(fm.fields))
	args := make([]any, 0, len(fm.fields)+len(whereArgs))
	for i, f := range fm.fields {
		sets[i] = f.column + " = ?"
		args = append(args, argOf(val.Field(f.index).Interface()))
	}
	args = append(args, normArgs(whereArgs)...)
	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		table, strings.Join(sets, ", "), where)
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, classify(err)
	}
	return res.RowsAffected()
}

// Delete removes rows matching the where expression and returns the
// number of rows affected, so callers can apply their own policy
// (e.g. n == 0 means "not found") without dropping to raw
// database/sql. The count comes from [database/sql.Result.RowsAffected].
//
// Security: table is validated as a SQL identifier (returns
// [ErrInvalidIdentifier] otherwise). where is interpolated verbatim —
// it is a trusted SQL fragment, not user input. Bind every value
// through a ? placeholder and pass it in whereArgs; never concatenate
// user input into where.
//
// where is not validated and must be non-empty: passing "" emits a
// dangling WHERE and yields a cryptic driver error. To delete every
// row deliberately, pass a constant true predicate such as "1 = 1".
func Delete(ctx context.Context, db Executor, table, where string, whereArgs ...any) (int64, error) {
	if err := checkTable("sqlx.Delete", table); err != nil {
		return 0, err
	}
	query := fmt.Sprintf("DELETE FROM %s WHERE %s", table, where)
	res, err := db.ExecContext(ctx, query, normArgs(whereArgs)...)
	if err != nil {
		return 0, classify(err)
	}
	return res.RowsAffected()
}

// buildInsertParts returns the column list, placeholder list, and
// bound args for [Insert]. It excludes the auto-PK column when its
// value is the zero value.
func buildInsertParts(fm *fieldMap, val reflect.Value) (cols []string, placeholders []string, args []any) {
	for i, f := range fm.fields {
		if i == fm.pkIndex && val.Field(f.index).IsZero() {
			continue
		}
		cols = append(cols, f.column)
		placeholders = append(placeholders, "?")
		args = append(args, argOf(val.Field(f.index).Interface()))
	}
	return cols, placeholders, args
}

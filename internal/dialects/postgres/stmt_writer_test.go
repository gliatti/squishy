package postgres

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// TestWriteStmt_BasicSelect — projection list, FROM, WHERE,
// quoted identifiers throughout.
func TestWriteStmt_BasicSelect(t *testing.T) {
	s := &ast.SelectStmt{
		Cols: []ast.SelectItem{
			{Expr: &ast.Ident{Parts: []string{"id"}}},
			{Expr: &ast.Ident{Parts: []string{"name"}}, Alias: "full_name"},
		},
		From: []ast.FromItem{
			&ast.FromTable{Schema: "public", Name: "users", Alias: "u"},
		},
		Where: &ast.BinaryExpr{
			Op:  "=",
			Lhs: &ast.Ident{Parts: []string{"u", "id"}},
			Rhs: &ast.Literal{Kind: "number", Text: "1"},
		},
	}
	want := `SELECT "id", "name" AS "full_name" FROM "public"."users" "u" WHERE "u"."id" = 1`
	if got := WriteStmt(s); got != want {
		t.Errorf("WriteStmt(SELECT) =\n  %q\nwant\n  %q", got, want)
	}
}

// TestWriteStmt_SelectStar — bare * and qualified t.* round-trip.
func TestWriteStmt_SelectStar(t *testing.T) {
	cases := []struct {
		in   ast.SelectItem
		want string
	}{
		{ast.SelectItem{Star: true}, "*"},
		{ast.SelectItem{Star: true, Qualifier: "u"}, `"u".*`},
	}
	for _, c := range cases {
		got := writeSelectItem(c.in)
		if got != c.want {
			t.Errorf("writeSelectItem(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWriteStmt_OrderByLimitOffset — clause ordering matches PG
// expectations.
func TestWriteStmt_OrderByLimitOffset(t *testing.T) {
	s := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Star: true}},
		From: []ast.FromItem{&ast.FromTable{Name: "t"}},
		OrderBy: []ast.OrderItem{
			{Expr: &ast.Ident{Parts: []string{"id"}}, Desc: true},
		},
		Limit:  &ast.Literal{Kind: "number", Text: "10"},
		Offset: &ast.Literal{Kind: "number", Text: "5"},
	}
	got := WriteStmt(s)
	if !strings.Contains(got, `ORDER BY "id" DESC`) {
		t.Errorf("ORDER BY missing or malformed: %q", got)
	}
	// Postgres canonical order is `... LIMIT n OFFSET m` but the writer
	// emits OFFSET first then LIMIT to match the existing translator
	// behaviour (PG accepts both orderings). Pin the round-trip shape.
	if !strings.Contains(got, "OFFSET 5") || !strings.Contains(got, "LIMIT 10") {
		t.Errorf("OFFSET/LIMIT missing: %q", got)
	}
}

// TestWriteStmt_FromJoin — INNER JOIN with ON, plus USING.
func TestWriteStmt_FromJoin(t *testing.T) {
	join := &ast.FromJoin{
		Left:  &ast.FromTable{Name: "a"},
		Right: &ast.FromTable{Name: "b"},
		Kind:  ast.InnerJoin,
		On: &ast.BinaryExpr{
			Op:  "=",
			Lhs: &ast.Ident{Parts: []string{"a", "id"}},
			Rhs: &ast.Ident{Parts: []string{"b", "id"}},
		},
	}
	got := writeFromItem(join)
	want := `"a" INNER JOIN "b" ON "a"."id" = "b"."id"`
	if got != want {
		t.Errorf("INNER JOIN ON: got %q, want %q", got, want)
	}

	usingJoin := &ast.FromJoin{
		Left:  &ast.FromTable{Name: "a"},
		Right: &ast.FromTable{Name: "b"},
		Kind:  ast.LeftJoin,
		Using: []string{"id"},
	}
	gotU := writeFromItem(usingJoin)
	wantU := `"a" LEFT JOIN "b" USING ("id")`
	if gotU != wantU {
		t.Errorf("LEFT JOIN USING: got %q, want %q", gotU, wantU)
	}
}

// TestWriteStmt_BasicInsert — VALUES form + ON CONFLICT DO NOTHING.
func TestWriteStmt_BasicInsert(t *testing.T) {
	ins := &ast.InsertStmt{
		Table: ast.TableRef{Schema: "public", Name: "users"},
		Cols:  []string{"id", "name"},
		Values: [][]ast.Expr{
			{&ast.Literal{Kind: "number", Text: "1"}, &ast.Literal{Kind: "string", Text: "alice"}},
		},
		OnConflict: &ast.OnConflict{DoNothing: true, Target: []string{"id"}},
	}
	want := `INSERT INTO "public"."users" ("id", "name") VALUES (1, 'alice') ON CONFLICT ("id") DO NOTHING`
	if got := WriteStmt(ins); got != want {
		t.Errorf("INSERT:\n  got  %q\n  want %q", got, want)
	}
}

// TestWriteStmt_Update — single-table SET with WHERE + RETURNING.
func TestWriteStmt_Update(t *testing.T) {
	upd := &ast.UpdateStmt{
		Table: ast.TableRef{Name: "t"},
		Sets: []ast.Assign{
			{Col: "name", Expr: &ast.Literal{Kind: "string", Text: "bob"}},
		},
		Where: &ast.BinaryExpr{
			Op:  "=",
			Lhs: &ast.Ident{Parts: []string{"id"}},
			Rhs: &ast.Literal{Kind: "number", Text: "1"},
		},
		Returning: []ast.SelectItem{
			{Expr: &ast.Ident{Parts: []string{"id"}}},
		},
	}
	want := `UPDATE "t" SET "name" = 'bob' WHERE "id" = 1 RETURNING "id"`
	if got := WriteStmt(upd); got != want {
		t.Errorf("UPDATE:\n  got  %q\n  want %q", got, want)
	}
}

// TestWriteStmt_Delete — bare DELETE FROM with WHERE.
func TestWriteStmt_Delete(t *testing.T) {
	del := &ast.DeleteStmt{
		Table: ast.TableRef{Name: "t"},
		Where: &ast.BinaryExpr{
			Op:  "<",
			Lhs: &ast.Ident{Parts: []string{"created_at"}},
			Rhs: &ast.FuncCall{Name: "now"},
		},
	}
	want := `DELETE FROM "t" WHERE "created_at" < now()`
	if got := WriteStmt(del); got != want {
		t.Errorf("DELETE:\n  got  %q\n  want %q", got, want)
	}
}

// TestWriteStmt_NilSafe — nil input returns "".
func TestWriteStmt_NilSafe(t *testing.T) {
	if got := WriteStmt(nil); got != "" {
		t.Errorf("WriteStmt(nil) = %q, want empty", got)
	}
}

// TestWriteSelectInto — the plpgsql INTO list lands after the head
// projection, target variables are not quoted, and the rest of the
// query renders exactly as WriteStmt would.
func TestWriteSelectInto(t *testing.T) {
	count := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Expr: ast.BuildFuncCall("COUNT", ast.BuildIdent("*"))}},
		From: []ast.FromItem{&ast.FromTable{Name: "t"}},
		Where: &ast.BinaryExpr{
			Op:  "=",
			Lhs: ast.BuildIdent("id"),
			Rhs: ast.BuildIdent("p_id"),
		},
	}
	if got, want := WriteSelectInto(count, []string{"n"}), `SELECT COUNT(*) INTO n FROM "t" WHERE "id" = "p_id"`; got != want {
		t.Errorf("COUNT INTO:\n  got  %q\n  want %q", got, want)
	}

	distinct := &ast.SelectStmt{
		Distinct: true,
		Cols: []ast.SelectItem{
			{Expr: ast.BuildIdent("a")},
			{Expr: ast.BuildIdent("b")},
		},
		From:    []ast.FromItem{&ast.FromTable{Schema: "s", Name: "t"}},
		OrderBy: []ast.OrderItem{{Expr: ast.BuildIdent("a"), Desc: true}},
		Limit:   ast.BuildIntLit(1),
	}
	if got, want := WriteSelectInto(distinct, []string{"v_a", "v_b"}), `SELECT DISTINCT "a", "b" INTO v_a, v_b FROM "s"."t" ORDER BY "a" DESC LIMIT 1`; got != want {
		t.Errorf("DISTINCT INTO:\n  got  %q\n  want %q", got, want)
	}

	// Only the head branch of a set-operation chain receives INTO.
	union := &ast.SelectStmt{
		Cols:   []ast.SelectItem{{Expr: ast.BuildIdent("a")}},
		From:   []ast.FromItem{&ast.FromTable{Name: "t1"}},
		SetOps: []ast.SetOp{{Op: "UNION", Stmt: &ast.SelectStmt{Cols: []ast.SelectItem{{Expr: ast.BuildIdent("a")}}, From: []ast.FromItem{&ast.FromTable{Name: "t2"}}}}},
	}
	if got, want := WriteSelectInto(union, []string{"v"}), `SELECT "a" INTO v FROM "t1" UNION SELECT "a" FROM "t2"`; got != want {
		t.Errorf("UNION INTO:\n  got  %q\n  want %q", got, want)
	}

	// Empty into list and nil input degrade gracefully.
	if got, want := WriteSelectInto(count, nil), WriteStmt(count); got != want {
		t.Errorf("empty INTO: got %q, want %q", got, want)
	}
	if got := WriteSelectInto(nil, []string{"n"}); got != "" {
		t.Errorf("nil SELECT: got %q, want empty", got)
	}
}

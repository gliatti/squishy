package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// These cases pin the MySQL routine-body path of TranslateRoutineBody:
// statements are parsed by the MySQL PL parser, rewritten on the AST by
// RewriteMySQLAST and rendered by the dialects/postgres writer. Nothing
// is re-lexed, and anything the parser could not type is reported
// instead of passing silently.
func TestMySQLRoutineBody(t *testing.T) {
	cases := []struct {
		name string
		// kind defaults to dialects.KindMySQL.
		kind dialects.Kind
		body string
		want []string
		// notWant are substrings that must not appear in the output.
		notWant []string
		// wantUntr are substrings each of which must appear in one of the
		// untranslated entries; noUntr asserts there are none.
		wantUntr []string
		noUntr   bool
	}{
		{
			name:    "trigger insert with NEW pseudo-record",
			body:    "BEGIN INSERT INTO orders_audit(order_id, action, at) VALUES (NEW.id, 'INS', NOW()); END",
			want:    []string{`INSERT INTO "orders_audit" ("order_id", "action", "at") VALUES (NEW."id", 'INS', now());`},
			notWant: []string{`"NEW"`},
			noUntr:  true,
		},
		{
			name:   "set ifnull",
			body:   "BEGIN DECLARE x INT; SET x = IFNULL(y, 0); END",
			want:   []string{`x := coalesce("y", 0);`},
			noUntr: true,
		},
		{
			name:    "null-safe equality in IF",
			body:    "BEGIN IF a <=> b THEN SET c = 1; END IF; END",
			want:    []string{`IF "a" IS NOT DISTINCT FROM "b" THEN`},
			notWant: []string{"<=>"},
			noUntr:  true,
		},
		{
			name: "declared cursor typed query",
			body: "BEGIN DECLARE cur CURSOR FOR SELECT id FROM t WHERE x <=> 1; OPEN cur; CLOSE cur; END",
			want: []string{`cur CURSOR FOR SELECT "id" FROM "t" WHERE "x" IS NOT DISTINCT FROM 1;`},
		},
		{
			name:   "select into typed",
			body:   "BEGIN DECLARE n INT; SELECT COUNT(*) INTO n FROM t WHERE id = p_id; END",
			want:   []string{`SELECT COUNT(*) INTO n FROM "t" WHERE "id" = "p_id";`},
			noUntr: true,
		},
		{
			name:    "group_concat in update subquery",
			body:    "BEGIN UPDATE t SET s = (SELECT GROUP_CONCAT(x SEPARATOR ',') FROM u); END",
			want:    []string{"string_agg("},
			notWant: []string{"GROUP_CONCAT", "SEPARATOR"},
			noUntr:  true,
		},
		{
			name:   "declare default rewritten",
			body:   "BEGIN DECLARE v INT DEFAULT IFNULL(p, 1); SET v = v + 1; END",
			want:   []string{`DEFAULT coalesce("p", 1);`},
			noUntr: true,
		},
		{
			// `<label>: BEGIN … END label;` becomes the PG
			// `<<label>>` block label (migrated from the legacy
			// text-level routine label tests).
			name:   "labelled block",
			body:   "BEGIN blk1: BEGIN SELECT 1; END blk1; END",
			want:   []string{"<<blk1>>"},
			noUntr: true,
		},
		{
			name:    "labelled while loop",
			body:    "BEGIN DECLARE x INT DEFAULT 3; loop1: WHILE x > 0 DO SET x = x - 1; END WHILE loop1; END",
			want:    []string{"<<loop1>>", `WHILE "x" > 0 LOOP`},
			notWant: []string{"END WHILE"},
			noUntr:  true,
		},
		{
			// An SQLEXCEPTION handler (whose action here is a bare
			// RESIGNAL) is not hoisted into an EXCEPTION block yet: it
			// must be reported, never passed through as MySQL text.
			name:     "exit handler reported",
			body:     "BEGIN DECLARE EXIT HANDLER FOR SQLEXCEPTION BEGIN RESIGNAL; END; SELECT 1; END",
			want:     []string{"-- TODO: DECLARE EXIT HANDLER FOR SQLEXCEPTION"},
			notWant:  []string{"RESIGNAL"},
			wantUntr: []string{"HANDLER FOR SQLEXCEPTION"},
		},
		{
			name:    "bare resignal",
			body:    "BEGIN RESIGNAL; END",
			want:    []string{"RAISE;"},
			notWant: []string{"RESIGNAL"},
			noUntr:  true,
		},
		{
			name: "resignal with sqlstate",
			body: "BEGIN RESIGNAL SQLSTATE '45000'; END",
			want: []string{"USING ERRCODE = '45000'"},
		},
		{
			name:   "commit verbatim",
			body:   "BEGIN UPDATE t SET a = 1; COMMIT; END",
			want:   []string{"COMMIT;"},
			noUntr: true,
		},
		{
			name:     "prepare copied verbatim with warning",
			body:     "BEGIN PREPARE s FROM 'x'; END",
			want:     []string{"PREPARE s FROM 'x'"},
			wantUntr: []string{"copied verbatim"},
		},
		{
			name:     "unparseable select into",
			body:     "BEGIN DECLARE n INT; SELECT a INTO n FROM t WHERE id = (1; END",
			wantUntr: []string{"could not be parsed"},
		},
		{
			name:   "truncate in body",
			body:   "BEGIN TRUNCATE TABLE t; END",
			want:   []string{`TRUNCATE TABLE "t";`},
			noUntr: true,
		},
		{
			name:   "get diagnostics verbatim",
			body:   "BEGIN DECLARE rc INT; DELETE FROM t WHERE id = 1; GET DIAGNOSTICS rc = ROW_COUNT; END",
			want:   []string{"GET DIAGNOSTICS rc = ROW_COUNT"},
			noUntr: true,
		},
		{
			// Locals are declared unquoted (PG folds them to lowercase);
			// references rendered by the writer must use the folded
			// spelling or they would resolve to a column "vCount".
			name: "mixed-case local variable",
			body: "BEGIN DECLARE vCount INT DEFAULT 0; SET vCount = vCount + 1; SELECT COUNT(*) INTO vCount FROM t WHERE a = vCount; END",
			want: []string{
				"vCount INTEGER DEFAULT 0;",
				`vCount := "vcount" + 1;`,
				`SELECT COUNT(*) INTO vCount FROM "t" WHERE "a" = "vcount";`,
			},
			notWant: []string{`"vCount"`},
			noUntr:  true,
		},
		{
			// MySQL resolves locals case-insensitively: every spelling
			// reaches the same declared variable.
			name:    "local referenced with another case",
			body:    "BEGIN DECLARE vCount INT; SET VCOUNT = vcount + VCount; END",
			want:    []string{`VCOUNT := "vcount" + "vcount";`},
			notWant: []string{`"VCount"`, `"VCOUNT"`},
			noUntr:  true,
		},
		{
			name: "upper-case DB2 locals",
			kind: dialects.KindDB2,
			body: "BEGIN DECLARE V_CNT INTEGER DEFAULT 0; DECLARE V_MAX INTEGER DEFAULT V_CNT + 10; SET V_CNT = V_CNT + 1; IF V_CNT > V_MAX THEN SET V_CNT = 0; END IF; END",
			want: []string{
				`V_MAX INTEGER DEFAULT "v_cnt" + 10;`,
				`V_CNT := "v_cnt" + 1;`,
				`IF "v_cnt" > "v_max" THEN`,
			},
			notWant: []string{`"V_CNT"`, `"V_MAX"`},
			noUntr:  true,
		},
		{
			name:    "local referenced in a cursor query",
			body:    "BEGIN DECLARE vMax INT DEFAULT 5; DECLARE cur CURSOR FOR SELECT id FROM t WHERE id < vMax; OPEN cur; CLOSE cur; END",
			want:    []string{`cur CURSOR FOR SELECT "id" FROM "t" WHERE "id" < "vmax";`},
			notWant: []string{`"vMax"`},
		},
		{
			// Columns (and routine parameters, whose signature is
			// rendered with quoteIdent) keep their source spelling: the
			// MySQL DDL path emits columns quoted with their source
			// case, so a body spelled like the table definition
			// resolves. A body spelling a column differently from its
			// DDL does not: MySQL folds column case, PG quoted names
			// do not.
			name:   "column and parameter case preserved",
			body:   "BEGIN UPDATE t SET ID = ID + 1 WHERE Name = pName; END",
			want:   []string{`UPDATE "t" SET "ID" = "ID" + 1 WHERE "Name" = "pName";`},
			noUntr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind := tc.kind
			if kind == "" {
				kind = dialects.KindMySQL
			}
			out, untr, _ := TranslateRoutineBody(tc.body, kind)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(out, nw) {
					t.Errorf("unexpected %q in:\n%s", nw, out)
				}
			}
			for _, wu := range tc.wantUntr {
				found := false
				for _, u := range untr {
					if strings.Contains(u, wu) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("no untranslated entry contains %q: %v", wu, untr)
				}
			}
			if tc.noUntr && len(untr) > 0 {
				t.Errorf("unexpected untranslated entries: %v\n%s", untr, out)
			}
		})
	}
}

// A MySQL parse error inside the body is surfaced as a note rather than
// dropped.
func TestMySQLRoutineBody_ParseErrorsNoted(t *testing.T) {
	_, _, notes := TranslateRoutineBody("BEGIN DECLARE n INT; SELECT a INTO n FROM t WHERE id = (1; END", dialects.KindMySQL)
	found := false
	for _, n := range notes {
		if strings.Contains(n, "MySQL parser reported errors") {
			found = true
		}
	}
	if !found {
		t.Errorf("parse errors not surfaced as a note: %v", notes)
	}
}

func TestCollectMySQLLocalVars(t *testing.T) {
	if got := collectMySQLLocalVars(nil); len(got) != 0 {
		t.Fatalf("nil body: %v", got)
	}
	inner := &ast.Block{Decls: []ast.PLDecl{&ast.DeclareVar{Name: "Été_X"}}}
	outer := &ast.Block{
		Decls: []ast.PLDecl{&ast.DeclareVar{Name: "a,vB"}},
		Stmts: []ast.PLStmt{inner},
	}
	got := collectMySQLLocalVars([]ast.PLStmt{outer})
	// PG folds ASCII letters only; MySQL matches case-insensitively.
	want := map[string]string{"a": "a", "vb": "vb", "été_x": "Été_x"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: got %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

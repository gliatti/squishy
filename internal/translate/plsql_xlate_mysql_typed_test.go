package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// These cases pin the routine translator on MySQL constructs that the
// hand-rolled parser now produces as typed nodes (aggregate modifiers,
// TRUNCATE, RESIGNAL, computed INTERVAL): none may be dropped or
// silently rewritten with a different meaning.
func TestTranslateRoutineBody_MySQLTypedNodes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		want     []string
		notWant  []string
		wantNote bool
		wantUntr bool
	}{
		{
			name:    "group_concat separator and order by",
			body:    "BEGIN UPDATE t SET s = (SELECT GROUP_CONCAT(v ORDER BY v SEPARATOR '|') FROM u); END",
			want:    []string{`string_agg(CAST("v" AS text), '|' ORDER BY "v")`},
			notWant: []string{"GROUP_CONCAT", "','"},
		},
		{
			name: "group_concat distinct default separator",
			body: "BEGIN UPDATE t SET s = (SELECT GROUP_CONCAT(DISTINCT v) FROM u); END",
			want: []string{`string_agg(DISTINCT CAST("v" AS text), ',')`},
		},
		{
			name: "group_concat several args",
			body: "BEGIN UPDATE t SET s = (SELECT GROUP_CONCAT(a, b SEPARATOR '; ') FROM u); END",
			want: []string{`string_agg(CAST(concat("a", "b") AS text), '; ')`},
		},
		{
			name: "count distinct keeps distinct",
			body: "BEGIN UPDATE t SET c = (SELECT COUNT(DISTINCT x) FROM u); END",
			want: []string{`COUNT(DISTINCT "x")`},
		},
		{
			name: "truncate in body",
			body: "BEGIN TRUNCATE TABLE t; TRUNCATE `Foo`; END",
			want: []string{`TRUNCATE TABLE "t";`, `TRUNCATE TABLE "Foo";`},
		},
		{
			name:    "bare resignal re-raises",
			body:    "BEGIN RESIGNAL; END",
			want:    []string{"RAISE;"},
			notWant: []string{"signalled from migrated routine", "RAISE EXCEPTION"},
		},
		{
			name:     "resignal sqlstate keeps message",
			body:     "BEGIN RESIGNAL SQLSTATE '45000'; END",
			want:     []string{"RAISE EXCEPTION USING ERRCODE = '45000', MESSAGE = SQLERRM;"},
			wantNote: true,
		},
		{
			name:     "resignal message keeps sqlstate",
			body:     "BEGIN RESIGNAL SET MESSAGE_TEXT = 'boom'; END",
			want:     []string{"RAISE EXCEPTION USING ERRCODE = SQLSTATE, MESSAGE = 'boom';"},
			wantNote: true,
		},
		{
			name: "signal unchanged",
			body: "BEGIN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'bad'; END",
			want: []string{"RAISE EXCEPTION 'bad' USING ERRCODE = '45000';"},
		},
		{
			name:    "computed interval is scaled",
			body:    "BEGIN UPDATE t SET d = NOW() + INTERVAL (n + 1) DAY; END",
			want:    []string{`now() + ("n" + 1) * INTERVAL '1 day'`},
			notWant: []string{"INTERVAL ''"},
		},
		{
			name:    "niladic current_date / current_time",
			body:    "BEGIN UPDATE t SET e = CURRENT_DATE, f = CURRENT_TIME; END",
			want:    []string{`"e" = CURRENT_DATE`, `"f" = CURRENT_TIME`},
			notWant: []string{"CURRENT_TIME()", `"CURRENT_DATE"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, untr, notes, _ := TranslateRoutineBodyExtV(tc.body, dialects.KindMySQL, "", "")
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
			if (len(untr) > 0) != tc.wantUntr {
				t.Errorf("untranslated = %v", untr)
			}
			if (len(notes) > 0) != tc.wantNote {
				t.Errorf("notes = %v", notes)
			}
		})
	}
}

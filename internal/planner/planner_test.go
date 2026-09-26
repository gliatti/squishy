package planner

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// A view that references another view must depend on that view's step so
// creation is serialized — the employees sample trips this with
// current_dept_emp joining dept_emp_latest_date.
func TestPlan_ViewReferencesAnotherView(t *testing.T) {
	src := &inspect.SourceSchema{Database: "db", Tables: nil}
	pg := &translate.Result{
		Plan: translate.SchemaPlan{
			Views: []translate.PGView{
				{Schema: "mig", Name: "current_dept_emp",
					DDL:        `CREATE OR REPLACE VIEW "mig"."current_dept_emp" AS SELECT * FROM dept_emp_latest_date;`,
					SelectBody: `SELECT * FROM dept_emp_latest_date`},
				{Schema: "mig", Name: "dept_emp_latest_date",
					DDL:        `CREATE OR REPLACE VIEW "mig"."dept_emp_latest_date" AS SELECT 1;`,
					SelectBody: `SELECT 1`},
			},
		},
	}

	p := Build(src, pg, BuildOptions{})

	var cur, latest *Step
	for i := range p.Steps {
		switch p.Steps[i].Target {
		case "view:current_dept_emp":
			cur = &p.Steps[i]
		case "view:dept_emp_latest_date":
			latest = &p.Steps[i]
		}
	}
	require.NotNil(t, cur)
	require.NotNil(t, latest)

	require.True(t, containsUUID(cur.DependsOn, latest.ID),
		"current_dept_emp must depend on dept_emp_latest_date")
	require.False(t, containsUUID(latest.DependsOn, cur.ID),
		"dep edge must be one-way")
	require.Greater(t, cur.Level, latest.Level,
		"referring view must land on a higher DAG level")
}

// A view that doesn't reference anything else keeps only the fk dep.
func TestPlan_StandaloneViewHasNoExtraDeps(t *testing.T) {
	src := &inspect.SourceSchema{Database: "db"}
	pg := &translate.Result{
		Plan: translate.SchemaPlan{
			Views: []translate.PGView{
				{Schema: "mig", Name: "v_alone",
					DDL:        `CREATE OR REPLACE VIEW "mig"."v_alone" AS SELECT 1;`,
					SelectBody: `SELECT 1`},
			},
		},
	}
	p := Build(src, pg, BuildOptions{})
	var s *Step
	for i := range p.Steps {
		if strings.HasPrefix(p.Steps[i].Target, "view:") {
			s = &p.Steps[i]
		}
	}
	require.NotNil(t, s)
	require.Len(t, s.DependsOn, 1, "only fk dep expected")
}

// viewSteps returns the create_routine steps of the two named views.
func viewSteps(t *testing.T, p *Plan, a, b string) (*Step, *Step) {
	t.Helper()
	var sa, sb *Step
	for i := range p.Steps {
		switch p.Steps[i].Target {
		case "view:" + a:
			sa = &p.Steps[i]
		case "view:" + b:
			sb = &p.Steps[i]
		}
	}
	require.NotNil(t, sa)
	require.NotNil(t, sb)
	return sa, sb
}

// A view translated from a typed body carries References; the planner
// must use them — here the DDL / SelectBody never mention the other view
// textually, so only the typed path can produce the edge.
func TestPlan_ViewReferencesTypedPath(t *testing.T) {
	src := &inspect.SourceSchema{Database: "db"}
	pg := &translate.Result{
		Plan: translate.SchemaPlan{
			Views: []translate.PGView{
				{Schema: "mig", Name: "current_dept_emp",
					DDL:        `CREATE OR REPLACE VIEW "mig"."current_dept_emp" AS SELECT * FROM "some_alias_source";`,
					SelectBody: `SELECT * FROM some_alias_source`,
					References: []string{"Dept_Emp_Latest_Date"}},
				{Schema: "mig", Name: "dept_emp_latest_date",
					DDL:        `CREATE OR REPLACE VIEW "mig"."dept_emp_latest_date" AS SELECT 1;`,
					SelectBody: `SELECT 1`,
					References: []string{}},
			},
		},
	}
	p := Build(src, pg, BuildOptions{})
	cur, latest := viewSteps(t, p, "current_dept_emp", "dept_emp_latest_date")
	require.True(t, containsUUID(cur.DependsOn, latest.ID),
		"References (case-insensitive) must create the view→view edge")
	require.False(t, containsUUID(latest.DependsOn, cur.ID))
	require.Greater(t, cur.Level, latest.Level)
}

// References set but empty: the typed body reads no other view, so no
// edge is added even though the DDL text mentions the other view's name
// (inside a string literal and a comment) — the token walk is not used.
func TestPlan_ViewEmptyReferencesNoEdge(t *testing.T) {
	src := &inspect.SourceSchema{Database: "db"}
	pg := &translate.Result{
		Plan: translate.SchemaPlan{
			Views: []translate.PGView{
				{Schema: "mig", Name: "v_label",
					DDL: `CREATE OR REPLACE VIEW "mig"."v_label" AS SELECT 'dept_emp_latest_date' AS label; -- dept_emp_latest_date` +
						"\n" + `SELECT dept_emp_latest_date`,
					SelectBody: `SELECT dept_emp_latest_date`,
					References: []string{}},
				{Schema: "mig", Name: "dept_emp_latest_date",
					DDL:        `CREATE OR REPLACE VIEW "mig"."dept_emp_latest_date" AS SELECT 1;`,
					SelectBody: `SELECT 1`,
					References: []string{}},
			},
		},
	}
	p := Build(src, pg, BuildOptions{})
	lbl, latest := viewSteps(t, p, "v_label", "dept_emp_latest_date")
	require.False(t, containsUUID(lbl.DependsOn, latest.ID),
		"empty References must not fall back to the text token walk")
	require.Len(t, lbl.DependsOn, 1, "only fk dep expected")
}

func containsUUID(ids []uuid.UUID, target uuid.UUID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func TestReferencesIdent(t *testing.T) {
	routine := "CREATE OR REPLACE FUNCTION \"mig\".\"f\"() RETURNS int AS $body$\n" +
		"DECLARE\n" +
		"  v \"mig\".emp.sal%TYPE;\n" +
		"  n int := 0;\n" +
		"BEGIN\n" +
		"  <<outer>>\n" +
		"  SELECT count(*)::int INTO n FROM dept_emp_latest_date WHERE x = 'it''s';\n" +
		"  /* block comment */ RETURN n;\n" +
		"END;\n" +
		"$body$ LANGUAGE plpgsql;"
	cases := []struct {
		name  string
		body  string
		ident string
		want  bool
	}{
		{"exact word", `SELECT * FROM dept_emp`, "dept_emp", true},
		{"case-insensitive", `SELECT * FROM DEPT_EMP`, "dept_emp", true},
		{"no partial match", `SELECT * FROM dept_emp_latest_date`, "dept_emp", false},
		{"quoted", `SELECT * FROM "mig"."dept_emp"`, "dept_emp", true},
		{"string literal", `SELECT 'dept_emp' AS x`, "dept_emp", false},
		{"line comment", "SELECT 1 -- dept_emp\n", "dept_emp", false},
		{"block comment", `SELECT 1 /* dept_emp */`, "dept_emp", false},
		{"keyword-named ident", `SELECT * FROM level`, "level", true},
		{"empty body", ``, "dept_emp", false},
		{"empty ident", `SELECT 1`, "", false},
		{"dollar-quoted routine matches body ident", routine, "dept_emp_latest_date", true},
		{"dollar-quoted routine %TYPE anchor", routine, "emp", true},
		{"dollar-quoted routine absent ident", routine, "current_dept_emp", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, referencesIdent(tc.body, tc.ident))
		})
	}
}

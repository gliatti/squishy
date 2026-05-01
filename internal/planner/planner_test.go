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

func containsUUID(ids []uuid.UUID, target uuid.UUID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

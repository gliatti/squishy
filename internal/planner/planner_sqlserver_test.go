package planner

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// SQL Server: each inspected table carries its T-SQL schema; the copy
// reads schema.table from the database and writes into the PG schema the
// translator mapped it to, and every step is keyed / targeted
// schema-qualified so two schemas may hold the same name.
func TestPlan_SQLServerPreservedSchemas(t *testing.T) {
	src := &inspect.SourceSchema{Database: "AdventureWorks", Tables: []inspect.ObjectSnapshot{
		{Name: "Address", Schema: "Person", Database: "AdventureWorks", Rows: 10},
		{Name: "Address", Schema: "Sales", Database: "AdventureWorks", Rows: 3},
		{Name: "SalesOrderHeader", Schema: "Sales", Database: "AdventureWorks", Rows: 5},
	}}
	pg := &translate.Result{Plan: translate.SchemaPlan{
		Tables: []translate.PGTable{
			{Schema: "person", Name: "address", SourceSchema: "Person", SourceName: "Address"},
			{Schema: "sales", Name: "address", SourceSchema: "Sales", SourceName: "Address"},
			{Schema: "sales", Name: "salesorderheader", SourceSchema: "Sales", SourceName: "SalesOrderHeader",
				CopyColumns: []string{"SalesOrderID", "SalesOrderNumber"}},
		},
		Views: []translate.PGView{
			{Schema: "sales", Name: "vorders", DDL: `CREATE OR REPLACE VIEW "sales"."vorders" AS SELECT 1;`, References: []string{}},
			{Schema: "person", Name: "vorders", DDL: `CREATE OR REPLACE VIEW "person"."vorders" AS SELECT 1;`, References: []string{}},
		},
		Routines: []translate.PGRoutine{
			{Kind: "procedure", Schema: "humanresources", Name: "uspupdate", DDL: "CREATE PROCEDURE …"},
		},
	}}
	p := Build(src, pg, BuildOptions{TargetSchema: "public", PreserveSourceSchemas: true})

	copies := map[string]*Step{}
	indexes := map[string]*Step{}
	targets := map[string]*Step{}
	var validate *Step
	for i := range p.Steps {
		s := &p.Steps[i]
		targets[s.Kind+"|"+s.Target] = s
		switch s.Kind {
		case "copy_table":
			copies[s.Target] = s
		case "create_index":
			indexes[s.Target] = s
		case "validate":
			validate = s
		}
	}
	require.Len(t, copies, 3)
	pa := copies["Person.Address"]
	require.NotNil(t, pa)
	require.Equal(t, "Person", pa.Payload["schema"])
	require.Equal(t, "Address", pa.Payload["table"])
	require.Equal(t, "person", pa.Payload["target_schema"])
	require.Nil(t, pa.Payload["columns"])
	sa := copies["Sales.Address"]
	require.Equal(t, "sales", sa.Payload["target_schema"])
	soh := copies["Sales.SalesOrderHeader"]
	require.Equal(t, []string{"SalesOrderID", "SalesOrderNumber"}, soh.Payload["columns"])
	require.Equal(t, int64(5), soh.RowsTotal)
	require.Equal(t, "person", indexes["Person.Address"].Payload["target_schema"])

	v := targets["create_routine|view:sales.vorders"]
	require.NotNil(t, v)
	require.Equal(t, "sales", v.Payload["target_schema"])
	require.NotNil(t, targets["create_routine|view:person.vorders"])
	r := targets["create_routine|procedure:humanresources.uspupdate"]
	require.NotNil(t, r)
	require.Equal(t, "humanresources", r.Payload["target_schema"])

	require.NotNil(t, validate)
	require.Equal(t, []string{}, validate.Payload["tables"])
	require.Equal(t, []ValidateTableCheck{
		{SourceSchema: "Person", SourceTable: "Address", TargetSchema: "person", TargetTable: "address"},
		{SourceSchema: "Sales", SourceTable: "Address", TargetSchema: "sales", TargetTable: "address"},
		{SourceSchema: "Sales", SourceTable: "SalesOrderHeader", TargetSchema: "sales", TargetTable: "salesorderheader"},
	}, validate.Payload["table_checks"])
}

// Snapshots without a schema (every other dialect) keep the historical
// payloads: database as source schema, the migration target schema, the
// bare table name as step target, no table_checks.
func TestPlan_SingleSchemaPayloadsUnchanged(t *testing.T) {
	src := &inspect.SourceSchema{Database: "shop", Tables: []inspect.ObjectSnapshot{{Name: "Orders", Database: "shop", Rows: 7}}}
	pg := &translate.Result{Plan: translate.SchemaPlan{
		Tables: []translate.PGTable{{Schema: "mig", Name: "Orders", CopyColumns: []string{"a"}}},
		Views:  []translate.PGView{{Schema: "mig", Name: "v", DDL: "CREATE VIEW v AS SELECT 1;"}},
	}}
	p := Build(src, pg, BuildOptions{TargetSchema: "mig"})
	for _, s := range p.Steps {
		switch s.Kind {
		case "copy_table":
			require.Equal(t, "Orders", s.Target)
			require.Equal(t, map[string]any{"schema": "shop", "table": "Orders", "rows": int64(7), "target_schema": "mig", "columns": []string{"a"}}, s.Payload)
		case "create_routine":
			require.Equal(t, "view:v", s.Target)
			require.Equal(t, "mig", s.Payload["target_schema"])
		case "validate":
			require.Equal(t, []string{"Orders"}, s.Payload["tables"])
			_, has := s.Payload["table_checks"]
			require.False(t, has)
		}
	}
}

// A SQL Server view reading a table-valued function waits for the
// function's step (CREATE VIEW checks the function exists); a function
// that itself reads a view keeps waiting for that view, without a cycle.
func TestPlan_SQLServerViewWaitsForFunction(t *testing.T) {
	src := &inspect.SourceSchema{Database: "squishy_sample"}
	pg := &translate.Result{Plan: translate.SchemaPlan{
		Views: []translate.PGView{
			{Schema: "sales", Name: "v_top", DDL: `CREATE OR REPLACE VIEW "sales"."v_top" AS SELECT * FROM "dbo"."customers" "c" LEFT JOIN LATERAL (SELECT 1 FROM "sales"."fn_orders_of"("c"."id") "f") "s" ON true;`, References: []string{"customers"}},
			{Schema: "dbo", Name: "v_base", DDL: `CREATE OR REPLACE VIEW "dbo"."v_base" AS SELECT 1 AS "x";`, References: []string{}},
		},
		Routines: []translate.PGRoutine{
			{Kind: "function", Schema: "sales", Name: "fn_orders_of", DDL: `CREATE OR REPLACE FUNCTION "sales"."fn_orders_of"("id" integer) RETURNS TABLE ("x" integer) LANGUAGE sql STABLE AS $b$ SELECT 1; $b$;`},
			{Kind: "function", Schema: "dbo", Name: "fn_reads_view", DDL: `CREATE OR REPLACE FUNCTION "dbo"."fn_reads_view"() RETURNS integer LANGUAGE sql AS $b$ SELECT "x" FROM "dbo"."v_base"; $b$;`},
		},
	}}
	stepByTarget := func(p *Plan) map[string]*Step {
		out := map[string]*Step{}
		for i := range p.Steps {
			out[p.Steps[i].Target] = &p.Steps[i]
		}
		return out
	}
	dependsOn := func(s *Step, id uuid.UUID) bool {
		for _, d := range s.DependsOn {
			if d == id {
				return true
			}
		}
		return false
	}

	p := Build(src, pg, BuildOptions{TargetSchema: "public", PreserveSourceSchemas: true})
	st := stepByTarget(p)
	vTop, vBase := st["view:sales.v_top"], st["view:dbo.v_base"]
	fnOf, fnRV := st["function:sales.fn_orders_of"], st["function:dbo.fn_reads_view"]
	require.True(t, dependsOn(vTop, fnOf.ID), "v_top reads fn_orders_of")
	require.Greater(t, vTop.Level, fnOf.Level)
	require.True(t, dependsOn(fnRV, vBase.ID), "fn_reads_view reads v_base")
	require.False(t, dependsOn(vBase, fnRV.ID), "no cycle")

	// Single-schema plans (MySQL / Oracle / DB2) keep their step graph.
	p = Build(src, pg, BuildOptions{TargetSchema: "mig"})
	st = stepByTarget(p)
	require.False(t, dependsOn(st["view:v_top"], st["function:fn_orders_of"].ID))
}

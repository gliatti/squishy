package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestCreateSequenceMinimal(t *testing.T) {
	src := "CREATE SEQUENCE s1;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	require.Len(t, stmts, 1)
	cs, ok := stmts[0].(*ast.CreateSequence)
	require.True(t, ok)
	require.Equal(t, "s1", cs.Name)
	require.False(t, cs.IfNotExists)
	require.False(t, cs.OrReplace)
	require.False(t, cs.Temporary)
}

func TestCreateSequenceFullMariaDBOptions(t *testing.T) {
	src := `CREATE OR REPLACE SEQUENCE IF NOT EXISTS s2
		START WITH 100
		INCREMENT BY 2
		MINVALUE 10
		MAXVALUE 1000
		CACHE 50
		CYCLE;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	require.Len(t, stmts, 1)
	cs := stmts[0].(*ast.CreateSequence)
	require.Equal(t, "s2", cs.Name)
	require.True(t, cs.OrReplace)
	require.True(t, cs.IfNotExists)
	require.True(t, cs.HasStart)
	require.EqualValues(t, 100, cs.Start)
	require.True(t, cs.HasIncr)
	require.EqualValues(t, 2, cs.Increment)
	require.True(t, cs.HasMin)
	require.EqualValues(t, 10, cs.MinValue)
	require.True(t, cs.HasMax)
	require.EqualValues(t, 1000, cs.MaxValue)
	require.True(t, cs.HasCache)
	require.EqualValues(t, 50, cs.Cache)
	require.True(t, cs.HasCycle)
	require.True(t, cs.Cycle)
}

func TestCreateSequenceNoVariants(t *testing.T) {
	src := `CREATE SEQUENCE s3 NOMINVALUE NOMAXVALUE NOCACHE NOCYCLE;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	cs := stmts[0].(*ast.CreateSequence)
	require.True(t, cs.NoMin)
	require.True(t, cs.NoMax)
	require.True(t, cs.NoCache)
	require.True(t, cs.HasCycle)
	require.False(t, cs.Cycle)
}

func TestCreateSequencePGStyleNoSpelling(t *testing.T) {
	src := `CREATE SEQUENCE s4 NO MINVALUE NO MAXVALUE NO CACHE NO CYCLE;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	cs := stmts[0].(*ast.CreateSequence)
	require.True(t, cs.NoMin)
	require.True(t, cs.NoMax)
	require.True(t, cs.NoCache)
	require.True(t, cs.HasCycle)
	require.False(t, cs.Cycle)
}

func TestCreateSequenceTemporary(t *testing.T) {
	src := `CREATE TEMPORARY SEQUENCE s5 START WITH 1 INCREMENT BY 1;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	cs := stmts[0].(*ast.CreateSequence)
	require.True(t, cs.Temporary)
	require.True(t, cs.HasStart)
	require.True(t, cs.HasIncr)
}

func TestCreateSequenceWithEngineTail(t *testing.T) {
	src := `CREATE SEQUENCE s6 START WITH 1 ENGINE=InnoDB;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	cs := stmts[0].(*ast.CreateSequence)
	require.Equal(t, "s6", cs.Name)
	require.True(t, cs.HasStart)
}

func TestCreateSequenceSchemaQualified(t *testing.T) {
	src := `CREATE SEQUENCE app.counter START WITH 1000;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	cs := stmts[0].(*ast.CreateSequence)
	require.Equal(t, "app", cs.Schema)
	require.Equal(t, "counter", cs.Name)
}

func TestCreateSequenceNegativeIncrement(t *testing.T) {
	src := `CREATE SEQUENCE s7 INCREMENT BY -5 START WITH 100;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	cs := stmts[0].(*ast.CreateSequence)
	require.True(t, cs.HasIncr)
	require.EqualValues(t, -5, cs.Increment)
}

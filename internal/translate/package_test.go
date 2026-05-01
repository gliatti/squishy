package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateCreatePackageEmitsBlockingPrereq(t *testing.T) {
	src := "DELIMITER //\n" +
		"CREATE PACKAGE my_pkg AS\n" +
		"  PROCEDURE p1(x INT);\n" +
		"END;//\n"
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})

	var found bool
	for _, p := range res.Prerequisites {
		if p.Severity == SeverityBlocking && strings.Contains(p.Title, "package") {
			found = true
			require.Contains(t, p.Object, "my_pkg")
		}
	}
	require.True(t, found, "expected a blocking package prerequisite")
}

func TestTranslateCreatePackageBodyEmitsBlockingPrereq(t *testing.T) {
	src := "DELIMITER //\n" +
		"CREATE PACKAGE BODY my_pkg AS\n" +
		"  PROCEDURE p1(x INT) AS BEGIN NULL; END;\n" +
		"END;//\n"
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})

	var found bool
	for _, p := range res.Prerequisites {
		if p.Severity == SeverityBlocking && strings.Contains(p.Title, "package") {
			found = true
		}
	}
	require.True(t, found)
}

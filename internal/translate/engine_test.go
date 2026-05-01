package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func warningMessages(res *Result) []string {
	out := make([]string, 0, len(res.Warnings))
	for _, w := range res.Warnings {
		out = append(out, w.Message)
	}
	return out
}

func TestTranslateAriaEngineEmitsTailoredWarning(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY) ENGINE=Aria;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	msgs := strings.Join(warningMessages(res), "\n")
	require.Contains(t, msgs, "Aria")
	require.Contains(t, msgs, "MyISAM")
}

func TestTranslateColumnStoreEngineMentionsOLAP(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY) ENGINE=ColumnStore;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	msgs := strings.Join(warningMessages(res), "\n")
	require.Contains(t, msgs, "ColumnStore")
	require.Contains(t, msgs, "OLAP")
}

func TestTranslateSpiderEngineMentionsSharding(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY) ENGINE=Spider;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	msgs := strings.Join(warningMessages(res), "\n")
	require.Contains(t, msgs, "Spider")
	require.Contains(t, msgs, "sharding")
}

func TestTranslateS3EngineMentionsDataNotCopied(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY) ENGINE=S3;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	msgs := strings.Join(warningMessages(res), "\n")
	require.Contains(t, msgs, "S3")
	require.Contains(t, msgs, "NOT copied")
}

func TestTranslateInnoDBEngineHasNoEngineWarning(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY) ENGINE=InnoDB;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	for _, w := range res.Warnings {
		require.NotEqual(t, "engine", w.Kind)
	}
}

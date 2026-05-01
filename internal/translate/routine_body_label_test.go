package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// `<label>: BEGIN … END label;` translates to `<<label>> BEGIN … END label;`
// (PG plpgsql syntax). The body rewriter recognises BEGIN-LOOP-WHILE.
func TestRewriteRoutineLabelsBeginEnd(t *testing.T) {
	src := `BEGIN
		blk1: BEGIN
		  SELECT 1;
		END blk1;
	END`
	out, _ := RewriteRoutineBody(src)
	require.Contains(t, out, "<<blk1>> BEGIN", "block label must become PG <<label>>")
	require.Contains(t, out, "END blk1;", "trailing END label must remain")
}

func TestRewriteRoutineLabelsLoop(t *testing.T) {
	src := `BEGIN
		loop1: WHILE x > 0 DO
		  SET x = x - 1;
		END WHILE loop1;
	END`
	out, _ := RewriteRoutineBody(src)
	require.Contains(t, out, "<<loop1>> WHILE", "loop label must wrap WHILE")
	// rewriteControlFlow has already handled WHILE → LOOP and END WHILE → END LOOP.
}

// RESIGNAL must lift to RAISE (bare) or RAISE EXCEPTION SQLSTATE '...'.
func TestRewriteResignalBare(t *testing.T) {
	src := `BEGIN
		DECLARE EXIT HANDLER FOR SQLEXCEPTION
		BEGIN
		  RESIGNAL;
		END;
	END`
	out, _ := RewriteRoutineBody(src)
	require.Contains(t, out, "RAISE;", "bare RESIGNAL must become RAISE;")
	require.NotContains(t, strings.ToUpper(out), "RESIGNAL")
}

func TestRewriteResignalWithSqlstate(t *testing.T) {
	src := `BEGIN
		RESIGNAL SQLSTATE '45000';
	END`
	out, _ := RewriteRoutineBody(src)
	require.Contains(t, out, "RAISE EXCEPTION SQLSTATE '45000';")
}

// GET DIAGNOSTICS rounds-trips: PG plpgsql has identical syntax.
func TestGetDiagnosticsPassesThrough(t *testing.T) {
	src := `BEGIN
		DELETE FROM t WHERE id = 1;
		GET DIAGNOSTICS rc = ROW_COUNT;
	END`
	out, _ := RewriteRoutineBody(src)
	require.Contains(t, out, "GET DIAGNOSTICS rc = ROW_COUNT")
}

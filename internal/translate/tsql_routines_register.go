package translate

import (
	"gitlab.com/dalibo/squishy/internal/dialects"
)

// tsql_routines_register.go — plugs the SQL Server routine-body
// translation (tsql_routines.go) into the translator core through the
// dialect_hooks.go registries. Kept apart from the translation code so
// the wiring is visible at a glance and tests can clear it
// (withTSQLHooks) without touching the passes themselves.
func init() {
	RegisterRoutineBodyParser(dialects.KindSQLServer, parseTSQLRoutineBody)
	RegisterRoutineBodyTranslator(dialects.KindSQLServer, translateTSQLRoutineBody)
}

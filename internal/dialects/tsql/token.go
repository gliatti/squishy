package tsql

// Position is a 1-based line/column position in the source input; Offset
// is the 0-based rune offset (the parser slices the []rune source with it,
// like the mysql package).
type Position struct {
	Line   int
	Col    int
	Offset int
}

type TokenKind int

const (
	TOK_EOF TokenKind = iota
	TOK_ERROR

	TOK_IDENT        // unquoted identifier (incl. #temp, ##global, $action)
	TOK_QUOTED_IDENT // [identifier] or "identifier" (QUOTED_IDENTIFIER ON)
	TOK_STRING       // 'abc' or N'abc' (Lit unescaped; NString on the token)
	TOK_NUMBER       // 42, 3.14, 1e10, .5
	TOK_HEX          // 0x1F (Lit = the hex digits, without 0x)
	TOK_MONEY        // $12.50 (Lit = the amount, without $)
	TOK_VARIABLE     // @local or @@SYSTEM (Lit as written, @ kept)
	TOK_KEYWORD      // reserved keyword, Lit = uppercase form
	TOK_PUNCT        // ( ) , ; . = <> != !< !> += :: etc.
	TOK_COMMENT      // -- ... or /* ... */ (nested)
	TOK_GO           // GO [count] batch separator on its own line (Lit = count or "")
	TOK_SQLCMD       // sqlcmd directive line (:setvar, :on error, :r …); Lit = upper-cased command
	TOK_SQLCMD_VAR   // $(Name) sqlcmd scripting variable reference (Lit = Name)
)

// Token carries the literal, the original source fragment and its span.
type Token struct {
	Kind TokenKind
	Lit  string // canonical literal (uppercase for keywords, unquoted for idents/strings)
	Raw  string // original source fragment
	Pos  Position
	// End is the rune offset just past the token.
	End int
	// LineStart is true when only whitespace precedes the token on its
	// line. Error recovery uses it to resynchronise on a statement keyword
	// that starts a line (T-SQL does not require statement terminators).
	LineStart bool
	// NString marks an N'...' national string literal.
	NString bool
}

func (k TokenKind) String() string {
	switch k {
	case TOK_EOF:
		return "EOF"
	case TOK_ERROR:
		return "ERROR"
	case TOK_IDENT:
		return "IDENT"
	case TOK_QUOTED_IDENT:
		return "QUOTED_IDENT"
	case TOK_STRING:
		return "STRING"
	case TOK_NUMBER:
		return "NUMBER"
	case TOK_HEX:
		return "HEX"
	case TOK_MONEY:
		return "MONEY"
	case TOK_VARIABLE:
		return "VARIABLE"
	case TOK_KEYWORD:
		return "KEYWORD"
	case TOK_PUNCT:
		return "PUNCT"
	case TOK_COMMENT:
		return "COMMENT"
	case TOK_GO:
		return "GO"
	case TOK_SQLCMD:
		return "SQLCMD"
	case TOK_SQLCMD_VAR:
		return "SQLCMD_VAR"
	}
	return "?"
}

// reserved is the T-SQL reserved keyword set: the "Basic keywords" section
// of reference/TSqlLexer.g4 is taken from the Microsoft reserved-keyword
// list (https://learn.microsoft.com/sql/t-sql/language-elements/reserved-keywords-transact-sql),
// i.e. the words that can only be used as identifiers when delimited
// ([x] / "x"). Every other word lexes as TOK_IDENT and the parser matches
// contextual keywords (NOCOUNT, TRY, CATCH, PERSISTED, …) case-insensitively
// on the identifier, exactly as the grammar's `keyword` rule lets them
// double as identifiers. The distinction matters for statement boundaries:
// T-SQL statements need no terminator, so an expression or an alias ends at
// the first reserved word.
var reserved = map[string]struct{}{}

func init() {
	for _, k := range []string{
		"ADD", "ALL", "ALTER", "AND", "ANY", "AS", "ASC", "AUTHORIZATION",
		"BACKUP", "BEGIN", "BETWEEN", "BREAK", "BROWSE", "BULK", "BY",
		"CASCADE", "CASE", "CHECK", "CHECKPOINT", "CLOSE", "CLUSTERED",
		"COALESCE", "COLLATE", "COLUMN", "COMMIT", "COMPUTE", "CONSTRAINT",
		"CONTAINS", "CONTAINSTABLE", "CONTINUE", "CONVERT", "CREATE", "CROSS",
		"CURRENT", "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP",
		"CURRENT_USER", "CURSOR",
		"DATABASE", "DBCC", "DEALLOCATE", "DECLARE", "DEFAULT", "DELETE",
		"DENY", "DESC", "DISK", "DISTINCT", "DISTRIBUTED", "DOUBLE", "DROP",
		"DUMP",
		"ELSE", "END", "ERRLVL", "ESCAPE", "EXCEPT", "EXEC", "EXECUTE",
		"EXISTS", "EXIT", "EXTERNAL",
		"FETCH", "FILE", "FILLFACTOR", "FOR", "FOREIGN", "FREETEXT",
		"FREETEXTTABLE", "FROM", "FULL", "FUNCTION",
		"GOTO", "GRANT", "GROUP",
		"HAVING", "HOLDLOCK",
		"IDENTITY", "IDENTITY_INSERT", "IDENTITYCOL", "IF", "IN", "INDEX",
		"INNER", "INSERT", "INTERSECT", "INTO", "IS",
		"JOIN",
		"KEY", "KILL",
		"LEFT", "LIKE", "LINENO", "LOAD",
		"MERGE",
		"NATIONAL", "NOCHECK", "NONCLUSTERED", "NOT", "NULL", "NULLIF",
		"OF", "OFF", "OFFSETS", "ON", "OPEN", "OPENDATASOURCE", "OPENQUERY",
		"OPENROWSET", "OPENXML", "OPTION", "OR", "ORDER", "OUTER", "OVER",
		"PERCENT", "PIVOT", "PLAN", "PRECISION", "PRIMARY", "PRINT", "PROC",
		"PROCEDURE", "PUBLIC",
		"RAISERROR", "READ", "READTEXT", "RECONFIGURE", "REFERENCES",
		"REPLICATION", "RESTORE", "RESTRICT", "RETURN", "REVERT", "REVOKE",
		"RIGHT", "ROLLBACK", "ROWCOUNT", "ROWGUIDCOL", "RULE",
		"SAVE", "SCHEMA", "SECURITYAUDIT", "SELECT",
		"SEMANTICKEYPHRASETABLE", "SEMANTICSIMILARITYDETAILSTABLE",
		"SEMANTICSIMILARITYTABLE", "SESSION_USER", "SET", "SETUSER",
		"SHUTDOWN", "SOME", "STATISTICS", "SYSTEM_USER",
		"TABLE", "TABLESAMPLE", "TEXTSIZE", "THEN", "TO", "TOP", "TRAN",
		"TRANSACTION", "TRIGGER", "TRUNCATE", "TRY_CONVERT", "TSEQUAL",
		"UNION", "UNIQUE", "UNPIVOT", "UPDATE", "UPDATETEXT", "USE", "USER",
		"VALUES", "VARYING", "VIEW",
		"WAITFOR", "WHEN", "WHERE", "WHILE", "WITH", "WRITETEXT",
	} {
		reserved[k] = struct{}{}
	}
}

func isReserved(upper string) bool {
	_, ok := reserved[upper]
	return ok
}

package db2

// Position is a 1-based line/column byte offset in the source input.
type Position struct {
	Line   int
	Col    int
	Offset int
}

type TokenKind int

const (
	TOK_EOF TokenKind = iota
	TOK_ERROR

	TOK_IDENT        // unquoted identifier (case-folded to upper for DB2 semantics)
	TOK_QUOTED_IDENT // "identifier" (case-preserved)
	TOK_STRING       // 'abc' (with '' escape) — also covers N'…' national string
	TOK_HEX_STRING   // X'ff…' / GX'…' / UX'…' / BX'…' (typed binary literal, see Lit prefix)
	TOK_NUMBER       // 42, 3.14, .5, 1.2E10
	TOK_KEYWORD      // reserved/contextual keyword, Lit = uppercase form
	TOK_PUNCT        // ( ) , ; . @ = != <> < <= > >= % | || ?
	TOK_COMMENT      // -- … or /* … */
	TOK_PARAM        // ? — DB2 dynamic SQL placeholder
	TOK_HOST_VAR     // :name (embedded SQL host variable)
	TOK_ASSIGN       // :=
	TOK_SLASH        // / — CLP script terminator (lone, line-start)
	TOK_LABEL_END    // : after an identifier as the loop/block label closer
)

// Token carries literal, keyword code, and position.
type Token struct {
	Kind TokenKind
	Lit  string // canonical literal (uppercase for keywords, stripped quotes for idents/strings)
	Raw  string // original raw source fragment
	Pos  Position
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
	case TOK_HEX_STRING:
		return "HEX_STRING"
	case TOK_NUMBER:
		return "NUMBER"
	case TOK_KEYWORD:
		return "KEYWORD"
	case TOK_PUNCT:
		return "PUNCT"
	case TOK_COMMENT:
		return "COMMENT"
	case TOK_PARAM:
		return "PARAM"
	case TOK_HOST_VAR:
		return "HOST_VAR"
	case TOK_ASSIGN:
		return ":="
	case TOK_SLASH:
		return "/"
	case TOK_LABEL_END:
		return "LABEL_END"
	}
	return "?"
}

// DB2 reserved + contextual keywords relevant to the subset we parse
// (DDL + SQL PL + routine bodies). Built from the upstream DB2 grammar
// keyword catalogue (grammars-v4 Db2Lexer.g4). Anything missing falls
// through to IDENT and can still be used as a column/identifier name.
var keywords = map[string]struct{}{}

func init() {
	for _, k := range []string{
		// DDL verbs
		"CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "TO", "OR", "REPLACE",
		"IF", "EXISTS", "NOT", "COMMENT", "LABEL", "DECLARE",
		// Object kinds
		"TABLE", "INDEX", "VIEW", "SEQUENCE", "TRIGGER", "PROCEDURE", "FUNCTION",
		"PACKAGE", "MODULE", "TYPE", "ALIAS", "SYNONYM", "TABLESPACE", "BUFFERPOOL",
		"DATABASE", "ROLE", "USER", "GROUP", "SCHEMA", "STOGROUP", "AUXILIARY",
		// Common clauses
		"ADD", "ALTER", "COLUMN", "CONSTRAINT", "MODIFY", "DISABLE", "ENABLE",
		"PRIMARY", "UNIQUE", "KEY", "FOREIGN", "REFERENCES", "ENFORCED", "TRUSTED",
		"ON", "DELETE", "UPDATE", "CASCADE", "RESTRICT", "NO", "ACTION", "SET", "DEFAULT", "NULL",
		"CHECK", "DEFERRABLE", "INITIALLY", "DEFERRED", "IMMEDIATE",
		"USING", "BTREE", "CLUSTER", "INCLUDE", "ALLOW", "DISALLOW", "REVERSE", "SCANS",
		"GLOBAL", "LOCAL", "TEMPORARY", "PARTITION", "PARTITIONED", "PARTITIONING",
		"BY", "RANGE", "HASH", "REPLICATION", "DISTRIBUTE",
		"VALUES", "STARTING", "ENDING", "INCLUSIVE", "EXCLUSIVE",
		"STORAGE", "ORGANIZE", "ORGANIZATION", "DATA", "CAPTURE", "CHANGES", "NONE",
		"COMPRESS", "COMPRESSION", "STATIC", "ADAPTIVE", "VALUE",
		"NOT_LOGGED", "LOGGED", "INITIALLY",
		"VOLATILE", "CARDINALITY",
		// Identity / sequence options
		"IDENTITY", "ALWAYS", "GENERATED", "BY", "DEFAULT",
		"START", "WITH", "INCREMENT", "MAXVALUE", "NO_MAXVALUE", "MINVALUE", "NO_MINVALUE",
		"NOMAXVALUE", "NOMINVALUE",
		"CYCLE", "NOCYCLE", "CACHE", "NOCACHE", "ORDER", "NO_ORDER", "NOORDER",
		// Types
		"SMALLINT", "INTEGER", "INT", "BIGINT", "DECIMAL", "DEC", "NUMERIC", "DECFLOAT",
		"REAL", "DOUBLE", "FLOAT", "PRECISION",
		"CHAR", "CHARACTER", "VARCHAR", "VARYING", "GRAPHIC", "VARGRAPHIC", "DBCLOB",
		"CLOB", "BLOB", "BINARY", "VARBINARY", "BIT", "FOR", "DATA",
		"DATE", "TIME", "TIMESTAMP", "WITH", "WITHOUT", "ZONE",
		"LONG", "ROWID", "XML", "BOOLEAN", "ARRAY", "CURSOR",
		// SQL PL keywords
		"BEGIN", "END", "ATOMIC",
		"IS", "AS", "RETURN", "RETURNS",
		"WHEN", "ELSE", "ELSEIF", "THEN", "CASE",
		"LOOP", "WHILE", "REPEAT", "UNTIL", "FOR", "IN", "OUT", "INOUT",
		"ITERATE", "LEAVE", "GOTO",
		"OPEN", "CLOSE", "FETCH",
		"SIGNAL", "RESIGNAL", "SQLSTATE", "SQLEXCEPTION", "SQLWARNING",
		"CONDITION", "HANDLER", "CONTINUE", "EXIT", "UNDO",
		"GET", "DIAGNOSTICS", "STACKED",
		"EXECUTE", "IMMEDIATE", "PREPARE", "STATEMENT",
		"CALL", "USING", "INTO",
		// Triggers
		"BEFORE", "AFTER", "INSTEAD", "OF", "EACH", "ROW", "REFERENCING",
		"NEW", "OLD", "NEW_TABLE", "OLD_TABLE", "MODE", "DB2SQL",
		"WHEN",
		// Routine modifiers
		"DETERMINISTIC", "READS", "MODIFIES", "CONTAINS", "SQL", "EXTERNAL",
		"LANGUAGE", "PARAMETER", "STYLE", "FENCED", "THREADSAFE", "PARALLEL",
		"DBINFO", "INHERIT", "SPECIFIC", "VARIANT",
		// DML-ish (captured raw in bodies but used by parser for triggers / RETURNING)
		"SELECT", "INSERT", "DELETE", "MERGE", "FROM", "WHERE",
		"COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE",
		"GROUP", "HAVING", "ORDER", "FETCH", "FIRST", "NEXT", "ROWS", "ONLY", "OFFSET", "LIMIT",
		"UNION", "ALL", "INTERSECT", "EXCEPT",
		"JOIN", "LEFT", "RIGHT", "FULL", "OUTER", "INNER", "CROSS", "NATURAL",
		"FINAL", "INTERMEDIATE", "OLD_TABLE", "NEW_TABLE",
		// Logical / boolean ops & literals
		"AND", "OR", "XOR", "LIKE", "BETWEEN", "ESCAPE", "IS",
		"DISTINCT", "ANY", "SOME", "TRUE", "FALSE", "UNKNOWN",
		// Window / group analytics
		"OVER", "PARTITION", "ROWS", "RANGE", "GROUPING", "SETS", "ROLLUP", "CUBE",
		// Cast / current-* functions
		"CAST", "TREAT", "XMLCAST",
		"CURRENT", "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP",
		"CURRENT_USER", "CURRENT_PATH", "CURRENT_SCHEMA", "CURRENT_SERVER",
		"SESSION_USER", "SYSTEM_USER", "USER",
		// Locking / isolation
		"LOCK", "SHARE", "MODE", "EXCLUSIVE", "WITH", "UR", "CS", "RS", "RR",
		"WAIT", "NOWAIT", "SKIP",
		// Privileges (noop)
		"GRANT", "REVOKE", "PRIVILEGES",
		// Misc
		"PUBLIC", "ASC", "DESC", "BOTH", "LEADING", "TRAILING",
		"MINUTE", "MINUTES", "HOUR", "HOURS", "SECOND", "SECONDS",
		"DAY", "DAYS", "MONTH", "MONTHS", "YEAR", "YEARS", "MICROSECOND", "MICROSECONDS",
	} {
		keywords[k] = struct{}{}
	}
}

func isKeyword(s string) bool {
	_, ok := keywords[s]
	return ok
}

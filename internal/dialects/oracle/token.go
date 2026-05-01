package oracle

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

	TOK_IDENT        // unquoted identifier (case-folded to upper for Oracle semantics)
	TOK_QUOTED_IDENT // "identifier" (case-preserved)
	TOK_STRING       // 'abc' or q'[...]' alternative-quoted
	TOK_NUMBER       // 42, 3.14, .5, 1e10, 5f (BINARY_FLOAT), 5d (BINARY_DOUBLE)
	TOK_HEX          // 0xFF (Oracle non-standard but tolerated)
	TOK_KEYWORD      // reserved / contextual keyword, Lit = uppercase form
	TOK_PUNCT        // ( ) , ; . @ = != % && || etc.
	TOK_COMMENT      // -- ... or /* ... */
	TOK_SLASH_TERM   // lone / at start of line — SQL*Plus statement terminator
	TOK_RAW          // used by parser for raw_block aggregation
	TOK_BIND         // :name or :1 (host variable)
	TOK_LABEL_START  // << (PL/SQL label opener)
	TOK_LABEL_END    // >> (PL/SQL label closer)
	TOK_ASSIGN       // :=
	TOK_ARROW        // =>
	TOK_RANGE        // ..
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
	case TOK_NUMBER:
		return "NUMBER"
	case TOK_HEX:
		return "HEX"
	case TOK_KEYWORD:
		return "KEYWORD"
	case TOK_PUNCT:
		return "PUNCT"
	case TOK_COMMENT:
		return "COMMENT"
	case TOK_SLASH_TERM:
		return "SLASH_TERM"
	case TOK_RAW:
		return "RAW"
	case TOK_BIND:
		return "BIND"
	case TOK_LABEL_START:
		return "LABEL_START"
	case TOK_LABEL_END:
		return "LABEL_END"
	case TOK_ASSIGN:
		return ":="
	case TOK_ARROW:
		return "=>"
	case TOK_RANGE:
		return ".."
	}
	return "?"
}

// Oracle reserved + contextual keywords relevant to the subset we parse
// (DDL + PL/SQL + routine bodies). Built from the PL/SQL grammar keyword
// catalogue (grammars-v4 PlSqlLexer.g4). Anything missing falls through to
// IDENT and can still be used as a column/identifier name.
var keywords = map[string]struct{}{}

func init() {
	for _, k := range []string{
		// DDL verbs
		"CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "TO",
		"OR", "REPLACE", "IF", "EXISTS", "NOT",
		// Object kinds
		"TABLE", "INDEX", "VIEW", "MATERIALIZED", "SEQUENCE", "SYNONYM", "TRIGGER",
		"PROCEDURE", "FUNCTION", "PACKAGE", "BODY", "TYPE", "DIRECTORY",
		"TABLESPACE", "USER", "ROLE", "DATABASE", "LINK", "EDITION", "CONTEXT",
		// Common clauses
		"ADD", "COLUMN", "CONSTRAINT", "MODIFY", "DISABLE", "ENABLE",
		"PRIMARY", "UNIQUE", "KEY", "FOREIGN", "REFERENCES",
		"ON", "DELETE", "UPDATE", "CASCADE", "RESTRICT", "NO", "ACTION", "SET", "DEFAULT",
		"CHECK", "DEFERRABLE", "INITIALLY", "DEFERRED", "IMMEDIATE", "VALIDATE", "NOVALIDATE",
		"USING", "BTREE", "BITMAP", "REVERSE", "COMPRESS", "NOCOMPRESS",
		"GLOBAL", "LOCAL", "TEMPORARY", "PARTITION", "SUBPARTITION",
		"BY", "RANGE", "LIST", "VALUES", "LESS", "THAN",
		"STORAGE", "ORGANIZATION", "HEAP", "EXTERNAL", "CLUSTER",
		"LOB", "STORE", "AS", "SECUREFILE", "BASICFILE",
		// Column props
		"NULL", "VISIBLE", "INVISIBLE", "IDENTITY", "ALWAYS", "GENERATED", "VIRTUAL", "STORED",
		"START", "WITH", "INCREMENT", "MAXVALUE", "NOMAXVALUE", "MINVALUE", "NOMINVALUE",
		"CYCLE", "NOCYCLE", "CACHE", "NOCACHE", "ORDER", "NOORDER", "KEEP", "SESSION",
		"COMMENT", "COLLATE",
		// Types
		"NUMBER", "NUMERIC", "DECIMAL", "DEC", "INTEGER", "INT", "SMALLINT",
		"FLOAT", "REAL", "DOUBLE", "PRECISION", "BINARY_FLOAT", "BINARY_DOUBLE",
		"CHAR", "VARCHAR", "VARCHAR2", "NCHAR", "NVARCHAR2", "CLOB", "NCLOB", "LONG", "RAW",
		"BLOB", "BFILE", "DATE", "TIMESTAMP", "INTERVAL", "YEAR", "MONTH",
		"DAY", "HOUR", "MINUTE", "SECOND", "ZONE", "TIME",
		"ROWID", "UROWID", "BOOLEAN", "XMLTYPE", "XML", "JSON",
		"VECTOR", "FLOAT32", "FLOAT64", "INT8", "BINARY",
		"BYTE", "CHAR_CS", "NCHAR_CS", "CHARACTER",
		// PL/SQL keywords
		"IS", "DECLARE", "BEGIN", "END", "EXCEPTION", "WHEN", "OTHERS", "RAISE",
		"THEN", "ELSE", "ELSIF", "CASE", "LOOP", "WHILE", "FOR", "IN", "OUT", "INOUT",
		"EXIT", "CONTINUE", "GOTO", "RETURN", "RETURNING", "BULK", "COLLECT", "INTO",
		"FORALL", "INDICES", "OF", "LIMIT", "SAVE", "EXCEPTIONS",
		"PRAGMA", "AUTONOMOUS_TRANSACTION", "EXCEPTION_INIT",
		"DETERMINISTIC", "PARALLEL_ENABLE", "PIPELINED", "RESULT_CACHE",
		"AUTHID", "CURRENT_USER", "DEFINER",
		"CURSOR", "OPEN", "CLOSE", "FETCH",
		"EXECUTE", "USING_NLS_COMP",
		// Triggers
		"BEFORE", "AFTER", "INSTEAD", "EACH", "ROW", "STATEMENT", "NEW", "OLD",
		"REFERENCING", "FOLLOWS", "PRECEDES", "COMPOUND",
		// DML-ish (captured raw in bodies but used by parser for triggers)
		"SELECT", "INSERT", "DELETE", "MERGE", "CALL", "COMMIT", "ROLLBACK", "SAVEPOINT",
		"FROM", "WHERE", "GROUP", "HAVING", "ORDER", "FETCH", "FIRST", "NEXT", "ROWS", "ONLY",
		"UNION", "ALL", "INTERSECT", "MINUS",
		"JOIN", "LEFT", "RIGHT", "FULL", "OUTER", "INNER", "CROSS", "NATURAL",
		// Synonym
		"PUBLIC",
		// Mviews / refresh
		"REFRESH", "COMPLETE", "FAST", "FORCE", "NOFORCE", "NEVER", "DEMAND", "BUILD",
		"EDITIONABLE", "NONEDITIONABLE",
		// Logical ops
		"AND", "OR_OP", "XOR", "LIKE", "BETWEEN", "ESCAPE",
		// DML extensions: select-spec hints, set ops, fetch / lock clause,
		// MERGE, error logging, window/group analytics keywords.
		"DISTINCT", "UNIQUE_NULLS", "RECURSIVE", "VALUE",
		"ROLLUP", "CUBE", "GROUPING", "SETS", "WINDOW", "OVER",
		"CAST", "EXCEPT", "MATCHED", "LOG", "ERRORS", "REJECT",
		"OFFSET", "TIES", "PERCENT", "NULLS", "LAST",
		"SHARE", "NOWAIT", "SKIP", "LOCKED", "LATERAL", "SIBLINGS",
		"CONNECT_BY_ROOT", "PARTITIONS",
		"ASC", "DESC", "WITHIN", "ROW", "WAIT", "UNLIMITED",
		"DUPLICATE", "DO", "NOTHING",
		// Literals
		"TRUE", "FALSE",
		// Misc Oracle
		"SYSDATE", "SYSTIMESTAMP", "CURRENT_TIMESTAMP", "CURRENT_DATE",
		"LOCALTIMESTAMP", "UID", "ROWNUM", "LEVEL", "CONNECT", "START_WITH", "PRIOR", "NOCOPY",
		"DUAL",
		// Synonym / grants (noop)
		"GRANT", "REVOKE", "ANALYZE",
	} {
		keywords[k] = struct{}{}
	}
	// Handle AND/OR/BETWEEN collisions: remove fake "OR_OP" and add real "OR" if missing.
	delete(keywords, "OR_OP")
	keywords["OR"] = struct{}{}
}

func isKeyword(s string) bool {
	_, ok := keywords[s]
	return ok
}

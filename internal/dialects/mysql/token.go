package mysql

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

	TOK_IDENT         // unquoted identifier
	TOK_QUOTED_IDENT  // `identifier`
	TOK_STRING        // 'abc' or "abc" (MySQL default)
	TOK_NUMBER        // 42, 3.14
	TOK_HEX           // X'FF' or 0xFF
	TOK_BIT           // b'01' or 0b01
	TOK_KEYWORD       // reserved keyword, Lit = uppercase form
	TOK_PUNCT         // ( ) , ; . @ = !=  etc.
	TOK_COMMENT       // -- ... or /* ... */ or # ...
	TOK_DELIMITER_CMD // DELIMITER // — client directive
	TOK_RAW           // used by parser for raw_block aggregation
)

// Token carries literal, keyword code, and position.
type Token struct {
	Kind TokenKind
	Lit  string // canonical literal (uppercase for keywords, stripped quotes for idents/strings)
	Raw  string // original raw source fragment (useful for raw_block preservation)
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
	case TOK_BIT:
		return "BIT"
	case TOK_KEYWORD:
		return "KEYWORD"
	case TOK_PUNCT:
		return "PUNCT"
	case TOK_COMMENT:
		return "COMMENT"
	case TOK_DELIMITER_CMD:
		return "DELIMITER_CMD"
	case TOK_RAW:
		return "RAW"
	}
	return "?"
}

// keyword set — covers DDL + routines vocabulary. Anything missing is an IDENT.
var keywords = map[string]struct{}{}

func init() {
	for _, k := range []string{
		"CREATE", "TABLE", "TEMPORARY", "IF", "NOT", "EXISTS", "DROP",
		"ALTER", "ADD", "COLUMN", "CONSTRAINT", "RENAME", "TO",
		"INDEX", "KEY", "PRIMARY", "UNIQUE", "FULLTEXT", "SPATIAL",
		"FOREIGN", "REFERENCES", "ON", "DELETE", "UPDATE", "CASCADE",
		"RESTRICT", "NO", "ACTION", "SET", "DEFAULT", "NULL",
		"AUTO_INCREMENT", "COMMENT", "COLLATE", "CHARACTER", "CHARSET",
		"ENGINE", "ROW_FORMAT", "KEY_BLOCK_SIZE", "USING", "BTREE", "HASH",
		"CHECK", "ENFORCED",
		"GENERATED", "ALWAYS", "AS", "VIRTUAL", "STORED", "INVISIBLE", "VISIBLE", "STORAGE", "PARSER",
		"DISK", "MEMORY",
		"UNSIGNED", "ZEROFILL",
		"TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT",
		"FLOAT", "DOUBLE", "REAL", "DECIMAL", "NUMERIC", "DEC",
		"CHAR", "VARCHAR", "TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT",
		"TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB",
		"BINARY", "VARBINARY",
		"DATE", "TIME", "DATETIME", "TIMESTAMP", "YEAR",
		"ENUM", "JSON", "BIT",
		"GEOMETRY", "POINT", "LINESTRING", "POLYGON", "MULTIPOINT",
		"MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION",
		"CURRENT_TIMESTAMP", "CURRENT_DATE", "CURRENT_TIME", "NOW", "LOCALTIMESTAMP",
		"UUID",
		"TRUE", "FALSE",
		"AND", "OR", "XOR", "IS", "LIKE", "IN", "BETWEEN", "DIV", "MOD",
		"CAST",
		// DML — query specification, joins, set operations, locking.
		"DISTINCT", "DISTINCTROW", "FULL", "CROSS", "NATURAL", "RECURSIVE",
		"VALUE", "DUPLICATE", "ROLLUP", "WINDOW", "SHARE", "MODE",
		"LOW_PRIORITY", "HIGH_PRIORITY", "DELAYED", "QUICK", "STRAIGHT_JOIN",
		"LATERAL", "RETURNING", "EXCEPT", "INTERSECT", "MINUS", "OVER",
		"VIEW", "OR_REPLACE", "REPLACE", "ALGORITHM", "SQL", "SECURITY", "DEFINER", "INVOKER",
		"WITH", "CASCADED", "LOCAL", "OPTION",
		"TRIGGER", "BEFORE", "AFTER", "INSERT", "FOR", "EACH", "ROW",
		"FOLLOWS", "PRECEDES",
		"PROCEDURE", "FUNCTION", "RETURNS", "BEGIN", "END",
		"DECLARE", "OPEN", "CLOSE", "FETCH", "INTO", "LOOP", "LEAVE", "ITERATE",
		"WHILE", "DO", "REPEAT", "UNTIL", "IF", "THEN", "ELSE", "ELSEIF", "CASE", "WHEN",
		"CURSOR", "HANDLER", "CONTINUE", "EXIT", "FOUND", "SQLEXCEPTION", "SQLWARNING",
		"LANGUAGE", "DETERMINISTIC", "CONTAINS", "READS", "DATA", "MODIFIES",
		"IN", "OUT", "INOUT",
		"EVENT", "SCHEDULE", "EVERY", "STARTS", "ENDS", "AT", "COMPLETION", "PRESERVE",
		"ENABLE", "DISABLE", "SLAVE",
		"PERIOD", "SYSTEM", "SYSTEM_TIME", "VERSIONING", "WITHOUT", "START",
		"SECOND", "MINUTE", "HOUR", "DAY", "WEEK", "MONTH", "QUARTER",
		"INTERVAL",
		"IGNORE",
		"MODIFY", "CHANGE", "FIRST", "CONVERT", "LOCK", "FORCE", "VALIDATION",
		"KEYS", "DISCARD", "IMPORT", "REORGANIZE", "COALESCE", "EXCHANGE",
		"ANALYZE", "OPTIMIZE", "REBUILD", "REPAIR", "REMOVE", "UPGRADE", "TRUNCATE",
		"INSTANT", "INPLACE", "COPY", "NONE", "SHARED", "EXCLUSIVE",
		"ONLINE", "OFFLINE", "ALL",
		"PARTITION", "PARTITIONS", "SUBPARTITION", "SUBPARTITIONS",
		"RANGE", "LIST", "LINEAR", "COLUMNS", "VALUES", "LESS", "THAN", "MAXVALUE",
		"DIRECTORY", "MAX_ROWS", "MIN_ROWS", "TABLESPACE", "NODEGROUP", "UNION",
		"ASC", "DESC",
		"LOGFILE",
		"SERVER", "ROLE", "USER", "DATABASE", "SCHEMA",
		// MariaDB CREATE SEQUENCE (10.3+)
		"SEQUENCE", "INCREMENT", "MINVALUE", "NOMINVALUE",
		"NOMAXVALUE", "CYCLE", "NOCYCLE", "CACHE", "NOCACHE",
		// MariaDB Oracle-compat: CREATE PACKAGE / PACKAGE BODY
		"PACKAGE", "BODY",
		// MariaDB 11.x VECTOR data type + VECTOR INDEX
		"VECTOR",
		// MariaDB per-column transparent compression
		"COMPRESSED",
		"LOAD", "GRANT", "REVOKE", "FLUSH", "SIGNAL", "RESIGNAL",
		// SIGNAL / RESIGNAL operands and diagnostic-area item names
		// (MySQL 13.6.7.7.1). Required so parseSignal recognizes them as
		// keywords rather than identifiers — otherwise the SQLSTATE clause
		// and SET MESSAGE_TEXT trail would be left unconsumed and surface
		// as raw lines in the translated PG body.
		"SQLSTATE", "MESSAGE_TEXT", "MYSQL_ERRNO",
		"CLASS_ORIGIN", "SUBCLASS_ORIGIN",
		"CONSTRAINT_CATALOG", "CONSTRAINT_SCHEMA", "CONSTRAINT_NAME",
		"CATALOG_NAME", "SCHEMA_NAME", "TABLE_NAME", "COLUMN_NAME", "CURSOR_NAME",
		"RETURNED_SQLSTATE",
		"STOP", "XA", "PREPARE", "EXECUTE", "DEALLOCATE", "UNLOCK",
		"CHECKSUM", "RESET", "PURGE",
		"USE", "CALL", "SELECT", "FROM", "WHERE", "GROUP", "BY",
		"ORDER", "LIMIT", "OFFSET", "HAVING", "JOIN", "LEFT", "RIGHT", "INNER", "OUTER",
		"RETURN", "NEW", "OLD",
		"NAMES",
	} {
		keywords[k] = struct{}{}
	}
}

func isKeyword(s string) bool {
	_, ok := keywords[s]
	return ok
}

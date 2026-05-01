package postgres

import (
	"fmt"
	"strings"
)

// Write renders a sequence of statements as a PG DDL script. It is the
// symmetric counterpart to dialects/mysql's Parse: MySQL source text → AST,
// SchemaPlan → PG AST, PG AST → DDL text.
func Write(stmts []Stmt) string {
	var b strings.Builder
	for _, s := range stmts {
		writeStmt(&b, s)
	}
	return b.String()
}

func writeStmt(b *strings.Builder, s Stmt) {
	switch x := s.(type) {
	case *CreateSchema:
		b.WriteString("CREATE SCHEMA ")
		if x.IfNotExists {
			b.WriteString("IF NOT EXISTS ")
		}
		b.WriteString(qIdent(x.Name))
		b.WriteString(";\n")
	case *SetSearchPath:
		parts := []string{qIdent(x.Schema)}
		for _, a := range x.Additional {
			parts = append(parts, qIdent(a))
		}
		fmt.Fprintf(b, "SET search_path TO %s;\n", strings.Join(parts, ", "))
	case *CreateSequence:
		b.WriteString("CREATE SEQUENCE ")
		if x.IfNotExists {
			b.WriteString("IF NOT EXISTS ")
		}
		fmt.Fprintf(b, "%s.%s", qIdent(x.Schema), qIdent(x.Name))
		if x.As != "" {
			fmt.Fprintf(b, " AS %s", x.As)
		}
		b.WriteString(";\n")
	case *SelectSetval:
		fmt.Fprintf(b,
			"SELECT setval('%s', COALESCE((SELECT max(%s)::bigint FROM %s.%s), 1));\n",
			escSQL(fmt.Sprintf("%s.%s", qIdent(x.Schema), qIdent(x.SeqName))),
			qIdent(x.Column),
			qIdent(x.Schema), qIdent(x.TableName))
	case *CreateTable:
		writeCreateTable(b, x)
	case *CreatePartition:
		writeCreatePartition(b, x)
	case *CreateIndex:
		writeCreateIndex(b, x)
	case *AlterTableAddFK:
		writeAddFK(b, x)
	case *AlterTableValidateFK:
		fmt.Fprintf(b, "ALTER TABLE %s.%s VALIDATE CONSTRAINT %s;\n",
			qIdent(x.Schema), qIdent(x.Table), qIdent(x.Name))
	case *CommentOn:
		fmt.Fprintf(b, "COMMENT ON %s %s IS %s;\n",
			x.Object, x.Target, sqlLit(x.Body))
	case *CreateFunction:
		writeCreateFunction(b, x)
	case *CreateProcedure:
		writeCreateProcedure(b, x)
	case *CreateTrigger:
		writeCreateTrigger(b, x)
	case *CreateView:
		writeCreateView(b, x)
	case *Raw:
		b.WriteString(x.Text)
		if !strings.HasSuffix(x.Text, "\n") {
			b.WriteByte('\n')
		}
	}
}

func writeCreateTable(b *strings.Builder, t *CreateTable) {
	fmt.Fprintf(b, "CREATE TABLE ")
	if t.IfNotExists {
		b.WriteString("IF NOT EXISTS ")
	}
	fmt.Fprintf(b, "%s.%s (\n", qIdent(t.Schema), qIdent(t.Name))
	n := len(t.Columns)
	for i, c := range t.Columns {
		b.WriteString("  ")
		b.WriteString(qIdent(c.Name))
		b.WriteByte(' ')
		b.WriteString(c.Type)
		if c.Collation != "" {
			fmt.Fprintf(b, " COLLATE %s", c.Collation)
		}
		if c.Identity != IdentityNone {
			fmt.Fprintf(b, " GENERATED %s AS IDENTITY", c.Identity)
		}
		if c.NotNull {
			b.WriteString(" NOT NULL")
		}
		if c.Default != "" {
			fmt.Fprintf(b, " DEFAULT %s", c.Default)
		}
		if c.Generated != nil {
			fmt.Fprintf(b, " GENERATED ALWAYS AS (%s) STORED", c.Generated.Expr)
		}
		if c.Check != "" {
			fmt.Fprintf(b, " CHECK (%s)", c.Check)
		}
		if i < n-1 || len(t.PrimaryKey) > 0 || len(t.Checks) > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	if len(t.PrimaryKey) > 0 {
		cols := make([]string, len(t.PrimaryKey))
		for i, c := range t.PrimaryKey {
			cols[i] = qIdent(c)
		}
		fmt.Fprintf(b, "  PRIMARY KEY (%s)", strings.Join(cols, ","))
		if len(t.Checks) > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	for i, ck := range t.Checks {
		sep := ","
		if i == len(t.Checks)-1 {
			sep = ""
		}
		fmt.Fprintf(b, "  CHECK (%s)%s\n", ck, sep)
	}
	b.WriteString(")")
	if t.PartitionBy != nil {
		cols := make([]string, len(t.PartitionBy.Columns))
		for i, c := range t.PartitionBy.Columns {
			cols[i] = qIdent(c)
		}
		fmt.Fprintf(b, " PARTITION BY %s (%s)",
			strings.ToUpper(t.PartitionBy.Method), strings.Join(cols, ","))
	}
	b.WriteString(";\n")
}

func writeCreatePartition(b *strings.Builder, p *CreatePartition) {
	method := strings.ToUpper(p.Method)
	if method == "" {
		method = "RANGE" // historical default
	}
	fmt.Fprintf(b, "CREATE TABLE %s.%s PARTITION OF %s.%s",
		qIdent(p.Schema), qIdent(p.Name),
		qIdent(p.Schema), qIdent(p.ParentTable))
	switch method {
	case "RANGE":
		from := p.From
		if from == "" {
			from = "MINVALUE"
		}
		to := p.To
		if to == "" {
			to = "MAXVALUE"
		}
		fmt.Fprintf(b, " FOR VALUES FROM (%s) TO (%s)", from, to)
	case "LIST":
		if p.IsDefault {
			b.WriteString(" FOR VALUES IN (DEFAULT)")
		} else {
			b.WriteString(" FOR VALUES IN (")
			b.WriteString(strings.Join(p.Values, ","))
			b.WriteString(")")
		}
	case "HASH":
		fmt.Fprintf(b, " FOR VALUES WITH (MODULUS %d, REMAINDER %d)",
			p.Modulus, p.Remainder)
	}
	b.WriteString(";\n")
}

func writeCreateIndex(b *strings.Builder, idx *CreateIndex) {
	uq := ""
	if idx.Unique {
		uq = "UNIQUE "
	}
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		isExpr := i < len(idx.ColumnIsExpr) && idx.ColumnIsExpr[i]
		if isExpr {
			cols[i] = "(" + c + ")"
		} else {
			cols[i] = qIdent(c)
		}
		if i < len(idx.ColumnDirs) {
			switch strings.ToUpper(idx.ColumnDirs[i]) {
			case "DESC":
				cols[i] += " DESC"
			case "ASC":
				cols[i] += " ASC"
			}
		}
	}
	fmt.Fprintf(b, "CREATE %sINDEX %s ON %s.%s",
		uq, qIdent(idx.Name), qIdent(idx.Schema), qIdent(idx.Table))
	if idx.Method != "" {
		fmt.Fprintf(b, " USING %s", idx.Method)
	}
	fmt.Fprintf(b, " (%s);\n", strings.Join(cols, ","))
}

func writeAddFK(b *strings.Builder, fk *AlterTableAddFK) {
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = qIdent(c)
	}
	refCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		refCols[i] = qIdent(c)
	}
	fmt.Fprintf(b, "ALTER TABLE %s.%s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s.%s (%s)",
		qIdent(fk.Schema), qIdent(fk.Table), qIdent(fk.Name),
		strings.Join(cols, ","),
		qIdent(fk.RefSchema), qIdent(fk.RefTable),
		strings.Join(refCols, ","))
	if fk.OnDelete != "" {
		fmt.Fprintf(b, " ON DELETE %s", fk.OnDelete)
	}
	if fk.OnUpdate != "" {
		fmt.Fprintf(b, " ON UPDATE %s", fk.OnUpdate)
	}
	if fk.Deferrable {
		b.WriteString(" DEFERRABLE")
		if fk.InitiallyDeferred {
			b.WriteString(" INITIALLY DEFERRED")
		}
	}
	if fk.NotValid {
		b.WriteString(" NOT VALID")
	}
	b.WriteString(";\n")
}

func writeCreateFunction(b *strings.Builder, f *CreateFunction) {
	lang := f.Language
	if lang == "" {
		lang = "plpgsql"
	}
	sec := ""
	if strings.EqualFold(f.Security, "DEFINER") {
		sec = " SECURITY DEFINER"
	}
	vol := ""
	if f.Volatile != "" {
		vol = " " + f.Volatile
	}
	fmt.Fprintf(b,
		"CREATE OR REPLACE FUNCTION %s.%s(%s) RETURNS %s\nLANGUAGE %s%s%s AS $body$\n%s\n$body$;\n",
		qIdent(f.Schema), qIdent(f.Name),
		f.Params, f.Returns,
		lang, vol, sec, strings.TrimRight(f.Body, "\n"))
}

func writeCreateProcedure(b *strings.Builder, p *CreateProcedure) {
	lang := p.Language
	if lang == "" {
		lang = "plpgsql"
	}
	sec := ""
	if strings.EqualFold(p.Security, "DEFINER") {
		sec = " SECURITY DEFINER"
	}
	fmt.Fprintf(b,
		"CREATE OR REPLACE PROCEDURE %s.%s(%s)\nLANGUAGE %s%s AS $body$\n%s\n$body$;\n",
		qIdent(p.Schema), qIdent(p.Name),
		p.Params, lang, sec, strings.TrimRight(p.Body, "\n"))
}

func writeCreateView(b *strings.Builder, v *CreateView) {
	b.WriteString("CREATE OR REPLACE VIEW ")
	fmt.Fprintf(b, "%s.%s", qIdent(v.Schema), qIdent(v.Name))
	if len(v.Columns) > 0 {
		cols := make([]string, len(v.Columns))
		for i, c := range v.Columns {
			cols[i] = qIdent(c)
		}
		fmt.Fprintf(b, " (%s)", strings.Join(cols, ","))
	}
	b.WriteString(" AS\n")
	body := strings.TrimRight(strings.TrimRight(v.Body, ";"), "\n")
	b.WriteString(body)
	if v.CheckOption != "" {
		// PG accepts WITH [CASCADED|LOCAL] CHECK OPTION verbatim.
		fmt.Fprintf(b, "\nWITH %s", strings.TrimPrefix(strings.ToUpper(v.CheckOption), "WITH "))
	}
	b.WriteString(";\n")
	if v.Security != "" {
		fmt.Fprintf(b, "-- source view SQL SECURITY was %s (not modeled on PG views)\n", v.Security)
	}
}

func writeCreateTrigger(b *strings.Builder, t *CreateTrigger) {
	forEach := t.ForEach
	if forEach == "" {
		forEach = "ROW"
	}
	// Drop any same-named trigger first so re-running create_routine on a
	// schema where the previous attempt succeeded is idempotent. PG has
	// `CREATE OR REPLACE TRIGGER` since 14, but DROP-then-CREATE is more
	// portable across the 13 .. 17 range squishy supports and matches the
	// idempotency pattern already used for views and functions in this
	// writer.
	fmt.Fprintf(b, "DROP TRIGGER IF EXISTS %s ON %s.%s;\n",
		qIdent(t.Name), qIdent(t.Schema), qIdent(t.Table))
	fmt.Fprintf(b,
		"CREATE TRIGGER %s %s %s ON %s.%s\n  FOR EACH %s",
		qIdent(t.Name), strings.ToUpper(t.Timing), strings.ToUpper(t.Event),
		qIdent(t.Schema), qIdent(t.Table),
		forEach)
	if t.WhenCond != "" {
		fmt.Fprintf(b, " WHEN (%s)", t.WhenCond)
	}
	fmt.Fprintf(b, " EXECUTE FUNCTION %s.%s();\n", qIdent(t.Schema), qIdent(t.FnName))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func qIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// sqlLit renders a safely-escaped SQL string literal.
func sqlLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// escSQL escapes single quotes in a value that will be embedded inside an
// existing SQL literal (without adding surrounding quotes).
func escSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

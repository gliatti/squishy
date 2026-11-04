package postgres

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// xpath_writer.go — renders typed XQuery nodes (ast.XQ*) as the XPath
// 1.0 text PostgreSQL's XMLTABLE / xpath() / xpath_exists() evaluate
// (libxml2). The T-SQL rewriter has already brought the tree into the
// XPath 1.0 subset (explicit namespace prefixes, no XQuery-only
// function); the writer only spells it: value comparisons `eq` / `ne`
// / … become `=` / `!=` / …, and string literals are quoted with
// whichever quote they do not contain (XPath 1.0 has no escape), or
// built with concat() when they contain both.

// WriteXPath1 renders e as an XPath 1.0 expression ("" for nil).
func WriteXPath1(e ast.XQExpr) string {
	var b strings.Builder
	writeXQ(&b, e)
	return b.String()
}

var xqValueComp = map[string]string{"eq": "=", "ne": "!=", "lt": "<", "le": "<=", "gt": ">", "ge": ">="}

func writeXQ(b *strings.Builder, e ast.XQExpr) {
	switch x := e.(type) {
	case nil:
	case *ast.XQPath:
		if x.Root && len(x.Steps) == 0 {
			b.WriteString("/")
			return
		}
		for i, st := range x.Steps {
			switch {
			case st.Desc:
				b.WriteString("//")
			case i > 0 || x.Root:
				b.WriteString("/")
			}
			writeXQStep(b, st)
		}
	case *ast.XQContextItem:
		b.WriteString(".")
	case *ast.XQFuncCall:
		if x.Prefix != "" && x.Prefix != "fn" {
			b.WriteString(x.Prefix)
			b.WriteString(":")
		}
		b.WriteString(x.Name)
		b.WriteString("(")
		for i, a := range x.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeXQ(b, a)
		}
		b.WriteString(")")
	case *ast.XQLiteral:
		if !x.String {
			b.WriteString(x.Text)
			return
		}
		b.WriteString(xpathString(x.Text))
	case *ast.XQBinary:
		op := x.Op
		if v, ok := xqValueComp[op]; ok {
			op = v
		}
		writeXQ(b, x.Lhs)
		b.WriteString(" ")
		b.WriteString(op)
		b.WriteString(" ")
		writeXQ(b, x.Rhs)
	case *ast.XQUnary:
		b.WriteString(x.Op)
		writeXQ(b, x.Expr)
	case *ast.XQParen:
		b.WriteString("(")
		writeXQ(b, x.Inner)
		b.WriteString(")")
	}
}

func writeXQStep(b *strings.Builder, st *ast.XQStep) {
	switch {
	case st.Primary != nil:
		writeXQ(b, st.Primary)
	case st.Test == nil:
	case st.Axis == "parent" && st.Test.Kind == "node":
		b.WriteString("..")
	default:
		switch st.Axis {
		case "", "child":
		case "attribute":
			b.WriteString("@")
		default:
			b.WriteString(st.Axis)
			b.WriteString("::")
		}
		if st.Test.Kind == "name" {
			if st.Test.Prefix != "" {
				b.WriteString(st.Test.Prefix)
				b.WriteString(":")
			}
			b.WriteString(st.Test.Local)
		} else {
			b.WriteString(st.Test.Kind)
			b.WriteString("()")
		}
	}
	for _, p := range st.Preds {
		b.WriteString("[")
		writeXQ(b, p)
		b.WriteString("]")
	}
}

// xpathString quotes s as an XPath 1.0 string literal.
func xpathString(s string) string {
	hasDouble, hasSingle := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			hasDouble = true
		case '\'':
			hasSingle = true
		}
	}
	switch {
	case !hasDouble:
		return `"` + s + `"`
	case !hasSingle:
		return `'` + s + `'`
	}
	// Both quotes: concat() of double-quoted runs and '"' pieces.
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			if i > start {
				parts = append(parts, `"`+s[start:i]+`"`)
			}
			parts = append(parts, `'"'`)
			start = i + 1
		}
	}
	if start < len(s) {
		parts = append(parts, `"`+s[start:]+`"`)
	}
	return "concat(" + strings.Join(parts, ", ") + ")"
}

// writeFromXMLTable renders an XMLTABLE source.
func writeFromXMLTable(x *ast.FromXMLTable) string {
	var b strings.Builder
	b.WriteString("XMLTABLE(")
	if len(x.Namespaces) > 0 {
		b.WriteString("XMLNAMESPACES(")
		for i, ns := range x.Namespaces {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(sqlString(ns.URI))
			b.WriteString(" AS ")
			b.WriteString(qIdent(ns.Prefix))
		}
		b.WriteString("), ")
	}
	b.WriteString(sqlString(x.RowPath))
	b.WriteString(" PASSING (")
	b.WriteString(WriteExpr(x.Passing))
	b.WriteString(") COLUMNS ")
	for i, c := range x.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(qIdent(c.Name))
		if c.Ordinality {
			b.WriteString(" FOR ORDINALITY")
			continue
		}
		b.WriteString(" ")
		b.WriteString(c.Type)
		if c.Path != "" {
			b.WriteString(" PATH ")
			b.WriteString(sqlString(c.Path))
		}
	}
	b.WriteString(")")
	if x.Alias != "" {
		b.WriteString(" AS ")
		b.WriteString(qIdent(x.Alias))
	}
	return b.String()
}

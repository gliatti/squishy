package db2

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// parseDataType reads a DB2 column data type from the current position and
// returns the matching AST node. The parser folds keywords/idents to upper
// (lexer guarantees TOK_KEYWORD.Lit upper) so we compare on the upper form.
//
// Coverage:
//   SMALLINT|INTEGER|INT|BIGINT
//   DECIMAL|NUMERIC|DEC[(p[,s])]
//   DECFLOAT[(16|34)]
//   REAL | DOUBLE [PRECISION] | FLOAT[(p)]
//   CHAR[(n)] [FOR BIT DATA] | CHARACTER[(n)]
//   VARCHAR(n) [FOR BIT DATA] | CHARACTER VARYING(n) | CHAR VARYING(n)
//   GRAPHIC[(n)] | VARGRAPHIC(n) | DBCLOB[(n)]
//   CLOB[(n)] | BLOB[(n)]
//   BINARY(n) | VARBINARY(n)
//   DATE | TIME | TIMESTAMP[(n)] [WITH TIME ZONE]
//   XML | BOOLEAN | ROWID
//   LONG VARCHAR | LONG VARGRAPHIC
//   <user-defined-type-name> (qualified or bare)
func (p *Parser) parseDataType() ast.DataType {
	pos := p.cur.Pos

	// Match on Lit upper-case for both KEYWORD and IDENT.
	if p.cur.Kind != TOK_KEYWORD && p.cur.Kind != TOK_IDENT {
		// Could still be a qualified UDT through a quoted ident.
		if p.cur.Kind == TOK_QUOTED_IDENT {
			schema, name := p.parseQualifiedName()
			return &ast.UserDefinedType{Schema: schema, Name: name, P: astPos(pos)}
		}
		p.errorHere("expected data type", "TYPE")
		return nil
	}

	tok := strings.ToUpper(p.cur.Lit)
	switch tok {
	case "SMALLINT":
		p.advance()
		return &ast.IntType{Name: "SMALLINT", P: astPos(pos)}
	case "INTEGER", "INT":
		p.advance()
		return &ast.IntType{Name: "INTEGER", P: astPos(pos)}
	case "BIGINT":
		p.advance()
		return &ast.IntType{Name: "BIGINT", P: astPos(pos)}

	case "DECIMAL", "DEC", "NUMERIC":
		p.advance()
		t := &ast.DecimalType{Name: "NUMERIC", P: astPos(pos)}
		if p.isPunct("(") {
			p.advance()
			if v, ok := p.intLit(); ok {
				t.Precision = int(v)
				t.HasPrec = true
			}
			if p.isPunct(",") {
				p.advance()
				if v, ok := p.intLit(); ok {
					t.Scale = int(v)
				}
			}
			p.expectPunct(")")
		}
		return t

	case "DECFLOAT":
		p.advance()
		t := &ast.DB2DecFloatType{P: astPos(pos)}
		if p.isPunct("(") {
			p.advance()
			if v, ok := p.intLit(); ok {
				t.Width = int(v)
				t.HasWidth = true
			}
			p.expectPunct(")")
		}
		return t

	case "REAL":
		p.advance()
		return &ast.FloatType{Name: "REAL", P: astPos(pos)}

	case "DOUBLE":
		p.advance()
		if p.isKw("PRECISION") {
			p.advance()
		}
		return &ast.FloatType{Name: "DOUBLE", P: astPos(pos)}

	case "FLOAT":
		p.advance()
		t := &ast.FloatType{Name: "FLOAT", P: astPos(pos)}
		if p.isPunct("(") {
			p.advance()
			if v, ok := p.intLit(); ok {
				t.Precision = int(v)
				t.HasPS = true
			}
			p.expectPunct(")")
		}
		return t

	case "CHAR", "CHARACTER":
		p.advance()
		// CHARACTER VARYING / CHAR VARYING → VARCHAR
		if p.isKw("VARYING") {
			p.advance()
			return p.parseCharLengthAndForBit("VARCHAR", pos)
		}
		// CHAR LARGE OBJECT → CLOB
		if p.isKw("LARGE") {
			p.advance()
			if p.isKw("OBJECT") {
				p.advance()
			}
			return p.parseLobLength("CLOB", pos)
		}
		return p.parseCharLengthAndForBit("CHAR", pos)

	case "VARCHAR":
		p.advance()
		return p.parseCharLengthAndForBit("VARCHAR", pos)

	case "GRAPHIC":
		p.advance()
		return p.parseGraphicLen("GRAPHIC", pos)

	case "VARGRAPHIC":
		p.advance()
		return p.parseGraphicLen("VARGRAPHIC", pos)

	case "DBCLOB":
		p.advance()
		return p.parseGraphicLen("DBCLOB", pos)

	case "CLOB":
		p.advance()
		return p.parseLobLength("CLOB", pos)

	case "BLOB":
		p.advance()
		t := &ast.BlobType{Name: "BLOB", P: astPos(pos)}
		if p.isPunct("(") {
			_ = p.captureBalancedParens()
		}
		return t

	case "BINARY":
		p.advance()
		// BINARY [LARGE OBJECT] → BLOB on PG side.
		if p.isKw("LARGE") {
			p.advance()
			if p.isKw("OBJECT") {
				p.advance()
			}
			t := &ast.BlobType{Name: "BLOB", P: astPos(pos)}
			if p.isPunct("(") {
				_ = p.captureBalancedParens()
			}
			return t
		}
		t := &ast.BinaryType{Name: "BINARY", P: astPos(pos)}
		if p.isPunct("(") {
			p.advance()
			if v, ok := p.intLit(); ok {
				t.Length = int(v)
			}
			p.expectPunct(")")
		}
		return t

	case "VARBINARY":
		p.advance()
		t := &ast.BinaryType{Name: "VARBINARY", P: astPos(pos)}
		if p.isPunct("(") {
			p.advance()
			if v, ok := p.intLit(); ok {
				t.Length = int(v)
			}
			p.expectPunct(")")
		}
		return t

	case "DATE":
		p.advance()
		return &ast.DateType{P: astPos(pos)}

	case "TIME":
		p.advance()
		return &ast.TimeType{P: astPos(pos)}

	case "TIMESTAMP":
		p.advance()
		fsp := 6
		hasFsp := false
		if p.isPunct("(") {
			p.advance()
			if v, ok := p.intLit(); ok {
				fsp = int(v)
				hasFsp = true
			}
			p.expectPunct(")")
		}
		// optional WITH TIME ZONE → DB2TimestampTZType
		if p.isKw("WITH") {
			p.advance()
			if p.isKw("TIME") {
				p.advance()
			}
			if p.isKw("ZONE") {
				p.advance()
			}
			return &ast.DB2TimestampTZType{Fsp: fsp, HasFsp: hasFsp, P: astPos(pos)}
		}
		// WITHOUT TIME ZONE — explicit, equivalent to default.
		if p.isKw("WITHOUT") {
			p.advance()
			if p.isKw("TIME") {
				p.advance()
			}
			if p.isKw("ZONE") {
				p.advance()
			}
		}
		return &ast.TimestampType{Fsp: fsp, P: astPos(pos)}

	case "XML":
		p.advance()
		return &ast.DB2XmlType{P: astPos(pos)}

	case "BOOLEAN":
		p.advance()
		return &ast.DB2BooleanType{P: astPos(pos)}

	case "ROWID":
		p.advance()
		return &ast.DB2RowIDType{P: astPos(pos)}

	case "LONG":
		p.advance()
		// LONG VARCHAR | LONG VARGRAPHIC
		if p.isKw("VARCHAR") {
			p.advance()
			return &ast.CharType{Name: "VARCHAR", P: astPos(pos)} // TEXT-mapped downstream
		}
		if p.isKw("VARGRAPHIC") {
			p.advance()
			return &ast.DB2GraphicType{Name: "LONG VARGRAPHIC", P: astPos(pos)}
		}
		// Bare LONG (deprecated) — treat like CLOB.
		return &ast.ClobType{Long: true, P: astPos(pos)}

	case "ARRAY":
		// ARRAY[<bound>] OF <type> not supported here — capture and bail.
		p.advance()
		return &ast.UserDefinedType{Name: "ARRAY", P: astPos(pos)}
	}

	// Fallthrough: unknown type → assume user-defined type. Could be
	// `schema.name`, possibly with a length argument we discard.
	schema, name := p.parseQualifiedName()
	if p.isPunct("(") {
		_ = p.captureBalancedParens()
	}
	return &ast.UserDefinedType{Schema: schema, Name: name, P: astPos(pos)}
}

// parseCharLengthAndForBit parses optional `(n)` then optional `FOR BIT DATA`
// for CHAR / VARCHAR. Returns either *CharType or *DB2ForBitDataType wrapping
// it.
func (p *Parser) parseCharLengthAndForBit(name string, pos Position) ast.DataType {
	t := &ast.CharType{Name: name, P: astPos(pos)}
	if p.isPunct("(") {
		p.advance()
		if v, ok := p.intLit(); ok {
			t.Length = int(v)
			t.HasLength = true
		}
		// Optional `OCTETS|CODEUNITS16|CODEUNITS32` length-unit — strip.
		if p.isKwOrIdent("OCTETS") || p.isKwOrIdent("CODEUNITS16") || p.isKwOrIdent("CODEUNITS32") {
			p.advance()
		}
		p.expectPunct(")")
	}
	if p.isKw("FOR") {
		p.advance()
		if p.isKw("BIT") {
			p.advance()
			if p.isKw("DATA") {
				p.advance()
			}
			return &ast.DB2ForBitDataType{Inner: t, P: astPos(pos)}
		}
		// FOR SBCS|MIXED DATA — drop silently.
		if p.isKwOrIdent("SBCS") || p.isKwOrIdent("MIXED") {
			p.advance()
		}
		if p.isKw("DATA") {
			p.advance()
		}
	}
	return t
}

// parseGraphicLen parses optional `(n[ <unit>])` for GRAPHIC/VARGRAPHIC/DBCLOB.
func (p *Parser) parseGraphicLen(name string, pos Position) ast.DataType {
	t := &ast.DB2GraphicType{Name: name, P: astPos(pos)}
	if p.isPunct("(") {
		p.advance()
		if v, ok := p.intLit(); ok {
			t.Length = int(v)
			t.HasLength = true
		}
		// Optional unit suffix (K|M|G) for DBCLOB length. Lexer emits these
		// fused with the preceding number for `1K` etc. — but DB2 also
		// accepts a separated form `1024 K`. We strip a trailing unit ident.
		if p.cur.Kind == TOK_IDENT {
			lit := strings.ToUpper(p.cur.Lit)
			if lit == "K" || lit == "M" || lit == "G" {
				p.advance()
			}
		}
		p.expectPunct(")")
	}
	return t
}

// parseLobLength parses `(n[ K|M|G])` for CLOB sizes and returns a ClobType.
func (p *Parser) parseLobLength(name string, pos Position) ast.DataType {
	_ = name
	t := &ast.ClobType{P: astPos(pos)}
	if p.isPunct("(") {
		_ = p.captureBalancedParens()
	}
	return t
}

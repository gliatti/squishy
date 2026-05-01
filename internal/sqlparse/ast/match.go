package ast

import "strings"

// IsFuncCallNamed reports whether n is a *FuncCall whose Name matches
// `name` case-insensitively. Returns the typed pointer when matched so
// callers don't double type-assert.
//
// The match is exact on the full Name string — schema-qualified calls
// like "oracle.decode" must be passed verbatim. Use IsFuncCallAnyOf to
// check a small alternative set.
func IsFuncCallNamed(n Node, name string) (*FuncCall, bool) {
	fc, ok := n.(*FuncCall)
	if !ok {
		return nil, false
	}
	if !strings.EqualFold(fc.Name, name) {
		return nil, false
	}
	return fc, true
}

// IsFuncCallAnyOf returns the typed pointer when n is a *FuncCall whose
// Name matches any of `names` case-insensitively.
func IsFuncCallAnyOf(n Node, names ...string) (*FuncCall, bool) {
	fc, ok := n.(*FuncCall)
	if !ok {
		return nil, false
	}
	for _, name := range names {
		if strings.EqualFold(fc.Name, name) {
			return fc, true
		}
	}
	return nil, false
}

// IsBinaryOp returns the typed pointer when n is a *BinaryExpr with the
// given operator (case-insensitive — `MOD`, `AND`, `OR` round-trip
// regardless of source casing).
func IsBinaryOp(n Node, op string) (*BinaryExpr, bool) {
	bin, ok := n.(*BinaryExpr)
	if !ok {
		return nil, false
	}
	if !strings.EqualFold(bin.Op, op) {
		return nil, false
	}
	return bin, true
}

// FuncNameLower returns the lower-cased Name of a FuncCall, or "" when
// n isn't a FuncCall. Convenient for switch-on-name dispatch.
func FuncNameLower(n Node) string {
	fc, ok := n.(*FuncCall)
	if !ok {
		return ""
	}
	return strings.ToLower(fc.Name)
}

// IdentJoinedLower returns the lower-cased dotted form of an Ident
// (e.g. ["pkg","Func"] → "pkg.func"), or "" when n isn't an Ident.
// Useful for matching dictionary-view names (ALL_TABLES → "all_tables")
// without re-implementing the dot-join in every caller.
func IdentJoinedLower(n Node) string {
	id, ok := n.(*Ident)
	if !ok {
		return ""
	}
	parts := make([]string, len(id.Parts))
	for i, p := range id.Parts {
		parts[i] = strings.ToLower(p)
	}
	return strings.Join(parts, ".")
}

// IsLiteralKind reports whether n is a *Literal of the given kind
// ("string", "number", "null", "bool", "hex", "bit"). Returns the
// typed pointer when matched.
func IsLiteralKind(n Node, kind string) (*Literal, bool) {
	lit, ok := n.(*Literal)
	if !ok {
		return nil, false
	}
	if lit.Kind != kind {
		return nil, false
	}
	return lit, true
}

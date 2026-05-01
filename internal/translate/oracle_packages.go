package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// packageVar describes a package-level variable declared in an Oracle
// `CREATE PACKAGE pkg IS … END;` spec or body. Captured so routine bodies
// referencing `<pkg>.<name>` can be rewritten to a PG-side substitute that
// preserves the per-session semantics of Oracle package state.
type packageVar struct {
	Pkg   string // lower-case package name
	Name  string // lower-case variable name
	PG    string // PG type used to cast the GUC string back on read
}

// preCollectPackageVars scans the input statement list for Oracle
// `CREATE PACKAGE` / `CREATE PACKAGE BODY` declarations and populates
// `t.packageVars` with their non-cursor / non-routine variable
// declarations. Called as a pre-pass so that the main-loop routine
// translation can rewrite `<pkg>.<var>` references in any body, regardless
// of whether the package appeared before or after the routine in the
// source dump.
func (t *translator) preCollectPackageVars(stmts []ast.Stmt) {
	if t.packageVars == nil {
		t.packageVars = map[string][]packageVar{}
	}
	for _, s := range stmts {
		switch x := s.(type) {
		case *ast.CreatePackage:
			// Spec: scan the entire block. Variable declarations may be
			// interleaved with PROCEDURE / FUNCTION signatures (one per
			// statement, no body), so a single early stop would miss them.
			vars := extractPackageVars(strings.ToLower(x.Name), x.Spec, false)
			if len(vars) > 0 {
				t.packageVars[strings.ToLower(x.Name)] = append(t.packageVars[strings.ToLower(x.Name)], vars...)
			}
		case *ast.CreatePackageBody:
			// Body: stop at the first PROCEDURE / FUNCTION. Bodies open
			// with optional private state declarations and then dive into
			// implementations, which contain `;`-delimited statements that
			// look like variable declarations to a naive scanner.
			vars := extractPackageVars(strings.ToLower(x.Name), x.Body, true)
			if len(vars) > 0 {
				t.packageVars[strings.ToLower(x.Name)] = append(t.packageVars[strings.ToLower(x.Name)], vars...)
			}
		}
	}
}

// extractPackageVars parses an Oracle package spec/body text and returns
// the variable declarations at the package's top-level. Lines that introduce
// CURSORs / TYPEs / SUBTYPEs / PROCEDUREs / FUNCTIONs are skipped; what
// remains is a list of `<name> <type> […];` simple declarations.
//
// Implementation is intentionally text-based (no PL/SQL declaration parser)
// because Oracle package specs in the wild contain plenty of vendor
// extensions, %TYPE / %ROWTYPE references, and constant initialisers — we
// only need the (name, type) pair to build the PG GUC name and back-cast.
func extractPackageVars(pkg, src string, stopAtRoutine bool) []packageVar {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	// Strip block comments + line comments so they don't hide declarations.
	src = stripPLSQLComments(src)
	// Find the IS|AS keyword if present, scan after it.
	upper := strings.ToUpper(src)
	if idx := indexBoundedKeyword(upper, "IS"); idx >= 0 {
		src = src[idx+2:]
	} else if idx := indexBoundedKeyword(upper, "AS"); idx >= 0 {
		src = src[idx+2:]
	}

	var out []packageVar
	for _, line := range splitTopLevelStatements(src) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Stop the moment we hit the first procedure / function definition:
		// everything past that point is routine-body content where `;`-split
		// fragments look like variable declarations to a naive scanner but
		// are actually IF/THEN/CASE/SELECT/EXCEPTION etc. clauses. Package
		// state, by Oracle convention, is declared before any routine.
		head := strings.ToUpper(strings.Fields(line)[0])
		switch head {
		case "PROCEDURE", "FUNCTION":
			if stopAtRoutine {
				return out
			}
			// In a spec, procedure / function declarations are signatures
			// only (no body). Skip them and keep scanning for vars.
			continue
		case "CURSOR", "TYPE", "SUBTYPE", "PRAGMA", "EXCEPTION",
			"BEGIN", "END", "IF", "ELSIF", "ELSE", "FOR", "WHILE",
			"LOOP", "CASE", "WHEN", "THEN", "RETURN", "RAISE",
			"INSERT", "UPDATE", "DELETE", "SELECT", "MERGE", "COMMIT",
			"ROLLBACK", "SAVEPOINT", "OPEN", "FETCH", "CLOSE", "EXIT",
			"CONTINUE", "GOTO", "EXECUTE", "NULL":
			continue
		}
		// Variable form: ident [CONSTANT] type […];
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.ToLower(strings.Trim(fields[0], `"`))
		// Skip if the second word is EXCEPTION (e.g. `my_excp EXCEPTION;`)
		// — exception declarations are not variables.
		if strings.EqualFold(fields[1], "EXCEPTION") {
			continue
		}
		typeStart := 1
		if strings.EqualFold(fields[1], "CONSTANT") {
			typeStart = 2
		}
		if typeStart >= len(fields) {
			continue
		}
		// Take everything up to the first `:=`, `DEFAULT`, or `;` as the
		// type expression. Most package vars are simple (NUMBER, VARCHAR2(n),
		// PLS_INTEGER, BOOLEAN); a few use %TYPE / %ROWTYPE.
		typExpr := joinUntil(fields[typeStart:], "DEFAULT", ":=", ";")
		typExpr = strings.TrimRight(typExpr, "; \t")
		if typExpr == "" {
			continue
		}
		pgType := mapPackageVarType(typExpr)
		out = append(out, packageVar{
			Pkg:  pkg,
			Name: name,
			PG:   pgType,
		})
	}
	return out
}

// rewritePackageVarRefs replaces `<pkg>.<var>` references in a routine body
// with PG-side substitutes derived from the registry. Reads become
// `current_setting('squishy.<pkg>.<var>', true)::<pgtype>`; assignments
// become `PERFORM set_config('squishy.<pkg>.<var>', (<expr>)::text, false)`.
// Only known (pkg, var) pairs in `vars` are rewritten — a dotted reference
// that doesn't match a registered package variable is left as-is so that
// `pkg.fn(args)` calls (handled separately) and table.column references in
// SELECT statements aren't accidentally clobbered.
func rewritePackageVarRefs(body string, vars map[string][]packageVar) string {
	if len(vars) == 0 || body == "" {
		return body
	}
	// Build a quick lookup: lower(pkg.var) → pgType.
	lookup := map[string]string{}
	for pkg, vs := range vars {
		for _, v := range vs {
			lookup[pkg+"."+v.Name] = v.PG
		}
	}
	if len(lookup) == 0 {
		return body
	}

	// First pass: assignment form `<pkg>.<var> := <expr>;` — must be
	// rewritten before the read pass so the LHS is replaced with
	// `PERFORM set_config(...)`. The body is line-oriented enough in
	// Oracle's emitted form that a per-statement scan is robust.
	body = rewritePackageVarAssignments(body, lookup)
	// Second pass: bare reads.
	body = rewritePackageVarReads(body, lookup)
	return body
}

func rewritePackageVarAssignments(body string, lookup map[string]string) string {
	var out strings.Builder
	out.Grow(len(body))
	i := 0
	inStr := false
	for i < len(body) {
		c := body[i]
		if inStr {
			out.WriteByte(c)
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					out.WriteByte(body[i+1])
					i += 2
					continue
				}
				inStr = false
			}
			i++
			continue
		}
		if c == '\'' {
			out.WriteByte(c)
			inStr = true
			i++
			continue
		}
		// Try to match a `<ident>.<ident> :=` assignment LHS.
		if (isIdentStart(c) || c == '"') && (i == 0 || !isIdentChar(body[i-1])) {
			pkg, n1, ok1 := readIdentLower(body, i)
			if ok1 && n1 < len(body) && body[n1] == '.' {
				name, n2, ok2 := readIdentLower(body, n1+1)
				if ok2 {
					// Skip whitespace, then check for ':='.
					p := n2
					for p < len(body) && (body[p] == ' ' || body[p] == '\t' || body[p] == '\n' || body[p] == '\r') {
						p++
					}
					if p+1 < len(body) && body[p] == ':' && body[p+1] == '=' {
						if _, ok := lookup[pkg+"."+name]; ok {
							// Capture the RHS up to the next `;` at depth 0.
							rhsStart := p + 2
							rhsEnd, found := findStmtEnd(body, rhsStart)
							if found {
								rhs := strings.TrimSpace(body[rhsStart:rhsEnd])
								out.WriteString("PERFORM set_config('squishy.")
								out.WriteString(pkg)
								out.WriteString(".")
								out.WriteString(name)
								out.WriteString("', (")
								out.WriteString(rhs)
								out.WriteString(")::text, false)")
								i = rhsEnd
								continue
							}
						}
					}
				}
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

func rewritePackageVarReads(body string, lookup map[string]string) string {
	var out strings.Builder
	out.Grow(len(body))
	i := 0
	inStr := false
	for i < len(body) {
		c := body[i]
		if inStr {
			out.WriteByte(c)
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					out.WriteByte(body[i+1])
					i += 2
					continue
				}
				inStr = false
			}
			i++
			continue
		}
		if c == '\'' {
			out.WriteByte(c)
			inStr = true
			i++
			continue
		}
		if (isIdentStart(c) || c == '"') && (i == 0 || !isIdentChar(body[i-1])) {
			pkg, n1, ok1 := readIdentLower(body, i)
			if ok1 && n1 < len(body) && body[n1] == '.' {
				name, n2, ok2 := readIdentLower(body, n1+1)
				if ok2 {
					if pgType, ok := lookup[pkg+"."+name]; ok {
						// Suppress when followed by `(` (function call) or
						// `:=` (already handled by the assignment pass).
						p := n2
						for p < len(body) && (body[p] == ' ' || body[p] == '\t') {
							p++
						}
						if p < len(body) && body[p] == '(' {
							out.WriteByte(c)
							i++
							continue
						}
						out.WriteString("current_setting('squishy.")
						out.WriteString(pkg)
						out.WriteString(".")
						out.WriteString(name)
						out.WriteString("', true)::")
						out.WriteString(pgType)
						i = n2
						continue
					}
				}
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// readIdentLower reads a quoted or bare identifier starting at p; returns
// (lower-case text, end offset, ok).
func readIdentLower(s string, p int) (string, int, bool) {
	if p >= len(s) {
		return "", p, false
	}
	if s[p] == '"' {
		end := strings.IndexByte(s[p+1:], '"')
		if end < 0 {
			return "", p, false
		}
		return strings.ToLower(s[p+1 : p+1+end]), p + 1 + end + 1, true
	}
	if !isIdentStart(s[p]) {
		return "", p, false
	}
	q := p
	for q < len(s) && isIdentChar(s[q]) {
		q++
	}
	return strings.ToLower(s[p:q]), q, true
}

// findStmtEnd scans s from p forward and returns the position of the next
// `;` at parenthesis depth 0, ignoring single-quoted strings.
func findStmtEnd(s string, p int) (int, bool) {
	depth := 0
	inStr := false
	for i := p; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// stripPLSQLComments removes `--` line comments and `/* … */` block comments
// without touching string-literal contents. Used by extractPackageVars so a
// commented-out variable declaration isn't mistakenly registered.
func stripPLSQLComments(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	inStr := false
	for i < len(s) {
		c := s[i]
		if inStr {
			out.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					out.WriteByte(s[i+1])
					i += 2
					continue
				}
				inStr = false
			}
			i++
			continue
		}
		if c == '\'' {
			out.WriteByte(c)
			inStr = true
			i++
			continue
		}
		if c == '-' && i+1 < len(s) && s[i+1] == '-' {
			// Line comment to next \n.
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return out.String()
			}
			i += j
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			// Block comment.
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return out.String()
			}
			i += 2 + j + 2
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// indexBoundedKeyword returns the offset of the first occurrence of `kw` (already
// upper-case) in `upper` that's at a word boundary, or -1.
func indexBoundedKeyword(upper, kw string) int {
	off := 0
	for off < len(upper) {
		i := strings.Index(upper[off:], kw)
		if i < 0 {
			return -1
		}
		i += off
		left := i == 0 || !isIdentByte(upper[i-1])
		right := i+len(kw) == len(upper) || !isIdentByte(upper[i+len(kw)])
		if left && right {
			return i
		}
		off = i + len(kw)
	}
	return -1
}

// splitTopLevelStatements splits an Oracle declaration block into top-level
// statement-terminator-delimited slices. Parenthesis depth is tracked so a
// nested type definition like `RECORD(a NUMBER, b VARCHAR2(10))` isn't
// chopped at its inner `,`. Strings are preserved literally.
func splitTopLevelStatements(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			cur.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					cur.WriteByte(s[i+1])
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		if c == '\'' {
			cur.WriteByte(c)
			inStr = true
			continue
		}
		if c == '(' {
			depth++
		} else if c == ')' {
			if depth > 0 {
				depth--
			}
		}
		if c == ';' && depth == 0 {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// joinUntil rejoins fields with single spaces, stopping at the first field
// that matches one of `stops` (case-insensitive). Used to extract a simple
// type expression up to `DEFAULT` / `:=` / `;`.
func joinUntil(fields []string, stops ...string) string {
	stopSet := map[string]bool{}
	for _, s := range stops {
		stopSet[strings.ToUpper(s)] = true
	}
	var parts []string
	for _, f := range fields {
		if stopSet[strings.ToUpper(f)] {
			break
		}
		// Inline `:=` truncates the type even if not space-separated.
		if k := strings.Index(f, ":="); k >= 0 {
			f = f[:k]
			if f != "" {
				parts = append(parts, f)
			}
			break
		}
		parts = append(parts, f)
	}
	return strings.Join(parts, " ")
}

// mapPackageVarType converts an Oracle scalar type expression captured from
// a package spec to the closest PG type, used as a cast on the GUC read
// path. Only covers the scalar shapes that fit in a session-level setting
// (numbers / strings / dates); anchored types (%TYPE / %ROWTYPE) and
// composite collections fall back to TEXT and the user is expected to
// rework the routine manually.
func mapPackageVarType(expr string) string {
	upper := strings.ToUpper(strings.TrimSpace(expr))
	switch {
	case strings.HasPrefix(upper, "NUMBER"), strings.HasPrefix(upper, "INTEGER"),
		strings.HasPrefix(upper, "PLS_INTEGER"), strings.HasPrefix(upper, "BINARY_INTEGER"),
		strings.HasPrefix(upper, "DECIMAL"), strings.HasPrefix(upper, "DEC"),
		strings.HasPrefix(upper, "FLOAT"), strings.HasPrefix(upper, "REAL"),
		strings.HasPrefix(upper, "BINARY_FLOAT"), strings.HasPrefix(upper, "BINARY_DOUBLE"):
		return "numeric"
	case strings.HasPrefix(upper, "BOOLEAN"):
		return "boolean"
	case strings.HasPrefix(upper, "DATE"), strings.HasPrefix(upper, "TIMESTAMP"):
		return "timestamp"
	case strings.HasPrefix(upper, "VARCHAR"), strings.HasPrefix(upper, "NVARCHAR"),
		strings.HasPrefix(upper, "CHAR"), strings.HasPrefix(upper, "NCHAR"),
		strings.HasPrefix(upper, "CLOB"), strings.HasPrefix(upper, "NCLOB"),
		strings.HasPrefix(upper, "STRING"), strings.HasPrefix(upper, "RAW"):
		return "text"
	}
	// Anchored / unknown — back-cast as text and let PG complain at runtime
	// if the call site needed a tighter type.
	return "text"
}

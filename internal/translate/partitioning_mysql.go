package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// translateMysqlPartitioning lifts the structured ast.Partitioning captured
// from a MySQL/MariaDB CREATE TABLE into a PG-shaped PGPartitioning. It
// returns (partitioning, info, err). On error the table should be emitted
// unpartitioned and the error surfaced as a warning. `info` carries
// non-fatal degradations (e.g. SUBPARTITION dropped, KEY → HASH approximation)
// the translator should fold into Explanations/Warnings.
func translateMysqlPartitioning(p *ast.Partitioning) (*PGPartitioning, []string, error) {
	if p == nil {
		return nil, nil, nil
	}
	var notes []string

	method, cols, err := mysqlPartitionKey(p)
	if err != nil {
		return nil, notes, err
	}
	if p.Linear {
		notes = append(notes, "LINEAR "+method+": PG declarative partitioning has no linear-hash variant; emitted as plain "+method)
	}
	if p.Method == "KEY" {
		notes = append(notes, "PARTITION BY KEY mapped to PG HASH; MySQL hashes via MD5 over the column tuple while PG uses a column-type-specific hash, so distribution differs")
	}
	if p.Subpartition != nil {
		notes = append(notes, "SUBPARTITION BY clause dropped: PG declarative partitioning does not support subpartitions natively")
	}

	out := &PGPartitioning{Method: method, Columns: cols}

	switch method {
	case "RANGE":
		if err := buildMysqlRangePartitions(p, out); err != nil {
			return nil, notes, err
		}
	case "LIST":
		if err := buildMysqlListPartitions(p, out); err != nil {
			return nil, notes, err
		}
	case "HASH":
		if err := buildMysqlHashPartitions(p, out); err != nil {
			return nil, notes, err
		}
	default:
		return nil, notes, fmt.Errorf("unsupported partition method %q", method)
	}
	return out, notes, nil
}

// mysqlPartitionKey returns the PG method ("RANGE"|"LIST"|"HASH") and the
// list of partition-key column names. `RANGE COLUMNS` / `LIST COLUMNS`
// degrade to plain RANGE/LIST. KEY degrades to HASH. Single-column
// HASH/RANGE/LIST require ExprText to be a bare identifier — anything else is
// rejected as unsupported (PG accepts expression keys but quoting them
// through the existing writer is a separate scope).
func mysqlPartitionKey(p *ast.Partitioning) (string, []string, error) {
	switch p.Method {
	case "RANGE COLUMNS":
		if len(p.Columns) == 0 {
			return "", nil, fmt.Errorf("RANGE COLUMNS without column list")
		}
		return "RANGE", p.Columns, nil
	case "LIST COLUMNS":
		if len(p.Columns) == 0 {
			return "", nil, fmt.Errorf("LIST COLUMNS without column list")
		}
		return "LIST", p.Columns, nil
	case "KEY":
		if len(p.Columns) == 0 {
			return "", nil, fmt.Errorf("PARTITION BY KEY () with empty column list (PG cannot infer the PK)")
		}
		return "HASH", p.Columns, nil
	case "RANGE", "LIST", "HASH":
		col, err := simpleIdent(p.ExprText)
		if err != nil {
			return "", nil, fmt.Errorf("PARTITION BY %s (%s): %w (expression-form keys not supported yet)", p.Method, p.ExprText, err)
		}
		return p.Method, []string{col}, nil
	}
	return "", nil, fmt.Errorf("unknown partition method %q", p.Method)
}

// simpleIdent accepts a bare identifier (with optional backticks) and
// rejects expressions. We use this to gate which partition-function shapes
// are safe to emit through the writer's qIdent-wrapping logic.
func simpleIdent(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty expression")
	}
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		s = s[1 : len(s)-1]
	}
	for _, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return "", fmt.Errorf("not a bare identifier")
	}
	if s == "" {
		return "", fmt.Errorf("empty identifier")
	}
	return s, nil
}

func buildMysqlRangePartitions(p *ast.Partitioning, out *PGPartitioning) error {
	for _, d := range p.Definitions {
		if !d.HasLessThan {
			return fmt.Errorf("RANGE partition %q missing VALUES LESS THAN", d.Name)
		}
		if len(d.LessThan) != len(out.Columns) {
			return fmt.Errorf("RANGE partition %q: %d bound atom(s) but %d partition column(s)",
				d.Name, len(d.LessThan), len(out.Columns))
		}
		// PG accepts MAXVALUE per-tuple-column; pass each atom through verbatim
		// (MySQL bounds are SQL expressions PG already understands: numeric
		// literals, string literals, MAXVALUE, simple function calls).
		bound := strings.Join(d.LessThan, ",")
		out.Partitions = append(out.Partitions, PGPartition{
			Name:       strings.ToLower(d.Name),
			UpperBound: bound,
		})
	}
	if len(out.Partitions) == 0 {
		return fmt.Errorf("RANGE partitioning with no partition definitions")
	}
	return nil
}

func buildMysqlListPartitions(p *ast.Partitioning, out *PGPartitioning) error {
	multi := len(out.Columns) > 1
	for _, d := range p.Definitions {
		if !d.HasIn {
			return fmt.Errorf("LIST partition %q missing VALUES IN", d.Name)
		}
		var values []string
		switch {
		case multi:
			if len(d.InVectors) == 0 {
				return fmt.Errorf("LIST COLUMNS partition %q expects tuple values", d.Name)
			}
			for _, vec := range d.InVectors {
				if len(vec) != len(out.Columns) {
					return fmt.Errorf("LIST COLUMNS partition %q: tuple of %d values but %d columns",
						d.Name, len(vec), len(out.Columns))
				}
				values = append(values, "("+strings.Join(vec, ",")+")")
			}
		default:
			if len(d.InAtoms) == 0 {
				return fmt.Errorf("LIST partition %q expects atom values", d.Name)
			}
			values = append(values, d.InAtoms...)
		}
		out.Partitions = append(out.Partitions, PGPartition{
			Name:   strings.ToLower(d.Name),
			Values: values,
		})
	}
	if len(out.Partitions) == 0 {
		return fmt.Errorf("LIST partitioning with no partition definitions")
	}
	return nil
}

func buildMysqlHashPartitions(p *ast.Partitioning, out *PGPartitioning) error {
	// Three accepted shapes:
	//   1. Definitions list → use len(Definitions) as modulus, names from list.
	//   2. PARTITIONS N     → synthesise N children with names p_h0..p_h(N-1).
	//   3. Neither          → MySQL defaults to PARTITIONS 1; refuse instead
	//      of emitting a single-bucket HASH which is pathological under PG.
	switch {
	case len(p.Definitions) > 0:
		n := len(p.Definitions)
		for i, d := range p.Definitions {
			name := strings.ToLower(d.Name)
			if name == "" {
				name = fmt.Sprintf("p_h%d", i)
			}
			out.Partitions = append(out.Partitions, PGPartition{
				Name:      name,
				Modulus:   n,
				Remainder: i,
			})
		}
	case p.Count > 0:
		for i := 0; i < p.Count; i++ {
			out.Partitions = append(out.Partitions, PGPartition{
				Name:      fmt.Sprintf("p_h%d", i),
				Modulus:   p.Count,
				Remainder: i,
			})
		}
	default:
		return fmt.Errorf("HASH partitioning without PARTITIONS N or partition list")
	}
	return nil
}

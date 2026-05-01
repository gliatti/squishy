package ast

// ---------------------------------------------------------------------------
// Oracle partitioning AST — structured replacement for the raw text stored in
// CreateTable.Options.OraclePartitioning.
//
// The MySQL/MariaDB Partitioning struct (in ddl.go) is a separate type with a
// different shape; Oracle's grammar diverges enough (REFERENCE/SYSTEM
// methods, INTERVAL partitioning, sub-template, multi-column LIST, etc.) that
// trying to share a single struct produced fields that meant nothing in one
// dialect. Two structs, parsed by their respective dialect parser, walked by
// dialect-specific visitors.
//
// Once every Oracle parser path produces an OraclePartitionPlan,
// TableOptions.OraclePartitioning (the raw string) is removed.
// ---------------------------------------------------------------------------

// OraclePartitionMethod enumerates the supported PARTITION BY methods.
type OraclePartitionMethod int

const (
	OraclePartUnknown OraclePartitionMethod = iota
	OraclePartRange
	OraclePartList
	OraclePartHash
	OraclePartReference // unsupported in PG — translator emits a blocking prereq
	OraclePartSystem    // unsupported in PG — translator emits a blocking prereq
)

func (m OraclePartitionMethod) String() string {
	switch m {
	case OraclePartRange:
		return "RANGE"
	case OraclePartList:
		return "LIST"
	case OraclePartHash:
		return "HASH"
	case OraclePartReference:
		return "REFERENCE"
	case OraclePartSystem:
		return "SYSTEM"
	}
	return "UNKNOWN"
}

// OraclePartitionPlan is the structured PARTITION BY clause for an Oracle
// CREATE TABLE.
type OraclePartitionPlan struct {
	Method  OraclePartitionMethod
	Columns []string // partition-key columns; empty for SYSTEM

	// Interval — non-empty for `PARTITION BY RANGE (...) INTERVAL (...)`.
	// Stored as the structured expression (typically an IntervalLit or a
	// FuncCall wrapping NUMTOYMINTERVAL/NUMTODSINTERVAL).
	Interval Expr

	// HashCount — `PARTITIONS N` shorthand for HASH method. 0 means a
	// definition list is provided in Definitions instead.
	HashCount int

	// ReferenceConstraint — for REFERENCE partitioning, the FK constraint
	// name driving the partition split.
	ReferenceConstraint string

	Definitions  []OraclePartition
	Subpartition *OracleSubpartitionPlan
}

// OracleSubpartitionPlan — `SUBPARTITION BY ...` clause and optional template.
type OracleSubpartitionPlan struct {
	Method   OraclePartitionMethod
	Columns  []string
	HashCount int

	// Template captures `SUBPARTITION TEMPLATE (...)` definitions. Empty when
	// the source uses inline subpartitions inside each PARTITION clause.
	Template []OracleSubpartitionDef
}

// OraclePartition — a single `PARTITION name VALUES (...)` definition.
type OraclePartition struct {
	Name string

	// Bounds carries the upper bound for RANGE partitioning (one entry per
	// partition-key column, in declaration order) or the explicit value list
	// for LIST partitioning.
	Bounds []OraclePartitionBound

	// IsDefault — true for `PARTITION ... VALUES (DEFAULT)` (LIST only).
	IsDefault bool

	// Subpartitions — inline `SUBPARTITION sub VALUES ...` entries.
	Subpartitions []OracleSubpartitionDef
}

// OracleSubpartitionDef — a single SUBPARTITION definition (template or inline).
type OracleSubpartitionDef struct {
	Name   string
	Bounds []OraclePartitionBound
	IsDefault bool
}

// OraclePartitionBoundKind tags how a bound value should be emitted on the
// PostgreSQL side. The translator uses it to wrap the Value in the right PG
// cast (TIMESTAMP '...', DATE '...', INTERVAL '...', or a bare literal).
type OraclePartitionBoundKind int

const (
	BoundUnknown OraclePartitionBoundKind = iota
	BoundMaxValue                         // RANGE high bound `MAXVALUE`
	BoundNumber
	BoundString
	BoundDate       // TO_DATE('...', '...')
	BoundTimestamp  // TO_TIMESTAMP[_TZ]('...', '...')
	BoundInterval   // INTERVAL '...' UNIT
	BoundExpression // any other expression (passed through)
)

// OraclePartitionBound — one bound atom in a partition definition.
type OraclePartitionBound struct {
	Kind  OraclePartitionBoundKind
	Value Expr // nil for BoundMaxValue
}

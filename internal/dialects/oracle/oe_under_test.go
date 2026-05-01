package oracle

import (
	"strings"
	"testing"
)

// CREATE TYPE name UNDER parent (methods) — Oracle subtype declaration.
// Used heavily by OE schema (CATALOG_TYP, COMPOSITE_CATEGORY_TYP, etc.).
func TestParseCreateTypeUnderSubtype(t *testing.T) {
	ddl := `CREATE OR REPLACE TYPE "OE"."CATALOG_TYP"
 UNDER composite_category_typ
      (
    MEMBER FUNCTION getCatalogName RETURN VARCHAR2
       , OVERRIDING MEMBER FUNCTION category_describe RETURN VARCHAR2
      );`
	stmts, errs := Parse(ddl)
	if len(errs) > 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	if len(stmts) != 1 {
		t.Fatalf("expected 1 stmt, got %d", len(stmts))
	}
}

// CREATE VIEW name OF type [UNDER parent] [WITH OBJECT IDENTIFIER (col)] AS SELECT
// — Oracle object views, used in OE for OC_CUSTOMERS, OC_ORDERS, OC_CORPORATE_CUSTOMERS.
func TestParseCreateObjectView(t *testing.T) {
	cases := []string{
		`CREATE OR REPLACE VIEW "OE"."OC_CUSTOMERS" OF "OE"."CUSTOMER_TYP"
  WITH OBJECT IDENTIFIER (customer_id) AS
  SELECT c.customer_id, c.cust_first_name FROM customers c;`,
		`CREATE OR REPLACE VIEW "OE"."OC_CORPORATE_CUSTOMERS" OF "OE"."CORPORATE_CUSTOMER_TYP"
  UNDER oc_customers AS
  SELECT c.customer_id FROM customers c;`,
	}
	for i, src := range cases {
		stmts, errs := Parse(src)
		if len(errs) > 0 {
			t.Errorf("case %d unexpected parse errors: %v", i, errs)
		}
		if len(stmts) != 1 {
			t.Errorf("case %d expected 1 stmt, got %d", i, len(stmts))
		}
	}
}

// CREATE TRIGGER ... INSTEAD OF INSERT ON NESTED TABLE col OF parent FOR EACH ROW
// — used for OE's order_items_trg.
func TestParseTriggerOnNestedTable(t *testing.T) {
	ddl := `CREATE OR REPLACE TRIGGER "OE"."ORDERS_ITEMS_TRG" INSTEAD OF INSERT ON NESTED
 TABLE order_item_list OF oc_orders FOR EACH ROW
DECLARE
    prod  product_information_typ;
BEGIN
    SELECT DEREF(:NEW.product_ref) INTO prod FROM DUAL;
END;`
	stmts, errs := Parse(ddl)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors: %v", errs)
	}
	if len(stmts) != 1 {
		t.Errorf("expected 1 stmt, got %d", len(stmts))
	}
	// Sanity: error messages should not mention NESTED
	for _, e := range errs {
		if strings.Contains(strings.ToUpper(e.Error()), "NESTED") {
			t.Errorf("parser still trips on NESTED: %v", e)
		}
	}
}

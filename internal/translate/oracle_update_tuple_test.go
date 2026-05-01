package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// PRC_RAT_RGR uses Oracle's multi-column UPDATE SET tuple form:
//
//	UPDATE ABT
//	   SET (NUM_CTA_RGR_ABT, COD_TYP_ABT, ...) =
//	       (SELECT NUM_CTA_ABT, COD_TYP_ABT, ... FROM ABT WHERE ...)
//	 WHERE NUM_CTA_ABT = NV_NUM_CTA;
//
// PG accepts the same syntax. Before this fix the Oracle parser
// dropped each column after the first into a CALL stmt and emitted
// `SET "" = (NUM_CTA_RGR_ABT), "COD_TYP_ABT" =;` — a hard syntax
// error.
func TestUpdateTupleSet(t *testing.T) {
	src := `BEGIN
  UPDATE abt
     SET (num_cta_rgr_abt, cod_typ_abt, cod_ati) =
         (SELECT num_cta_abt, cod_typ_abt, cod_ati FROM abt WHERE num_cta_abt = nv_num_rgr)
   WHERE num_cta_abt = nv_num_cta;
END;`
	got, _, _ := TranslateRoutineBody(src, dialects.KindOracle)
	if !strings.Contains(strings.ToUpper(got), `("NUM_CTA_RGR_ABT", "COD_TYP_ABT", "COD_ATI")`) {
		t.Fatalf("tuple SET column list not preserved:\n%s", got)
	}
	if !strings.Contains(got, "(SELECT") {
		t.Fatalf("tuple SET subquery not preserved:\n%s", got)
	}
	// Sanity: the broken pre-fix output had "" or stray CALL — guard:
	if strings.Contains(got, `SET ""`) {
		t.Fatalf("zero-length identifier in SET — pre-fix bug:\n%s", got)
	}
	if strings.Contains(got, "CALL \"COD_ATI\"") {
		t.Fatalf("column dropped to a CALL stmt — pre-fix bug:\n%s", got)
	}
}

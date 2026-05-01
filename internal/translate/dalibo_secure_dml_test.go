package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

func TestSecureDMLProcedureBody(t *testing.T) {
	body := `BEGIN
  IF TIME_FORMAT(CURRENT_TIME, '%H:%i') NOT BETWEEN '08:00' AND '18:00'
        OR DAYNAME(CURRENT_DATE) IN ('Saturday', 'Sunday') THEN
    SIGNAL SQLSTATE '45000'
    SET MESSAGE_TEXT = 'You may only make changes during normal office hours';
  END IF;
END`
	pg, untrans, _ := TranslateRoutineBody(body, dialects.KindMySQL)
	t.Logf("=== translated ===\n%s\n=== untranslated: %v ===", pg, untrans)
	if strings.Contains(strings.ToUpper(pg), "SIGNAL SQLSTATE") {
		t.Fatalf("translation still contains literal SIGNAL SQLSTATE:\n%s", pg)
	}
	if !strings.Contains(strings.ToUpper(pg), "RAISE") {
		t.Fatalf("expected RAISE in translation:\n%s", pg)
	}
}

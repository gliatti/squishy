package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.com/dalibo/squishy/internal/project"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// getPrerequisites returns the prerequisites + current acknowledgement state
// for a migration. Used by the wizard Checklist step.
func (d *Deps) getPrerequisites(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "migrationID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid migrationID")
		return
	}
	m, err := d.Repo.GetMigration(r.Context(), id)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "migration not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var prereqs []translate.Prerequisite
	_ = json.Unmarshal(m.Prerequisites, &prereqs)
	var acked []string
	_ = json.Unmarshal(m.AckedPrereqs, &acked)

	ackSet := map[string]bool{}
	for _, id := range acked {
		ackSet[id] = true
	}
	unresolvedBlocking := 0
	for _, p := range prereqs {
		if p.Severity == translate.SeverityBlocking && !ackSet[p.ID] {
			unresolvedBlocking++
		}
	}

	okJSON(w, map[string]any{
		"migration_id":         m.ID,
		"prerequisites":        prereqs,
		"acked":                acked,
		"unresolved_blocking":  unresolvedBlocking,
		"can_launch":           unresolvedBlocking == 0,
	})
}

// ackPrerequisites replaces the acknowledged set with the provided list.
// Body: {"acked": ["id1","id2",...]}. A prereq is considered resolved once
// its ID appears in the acked list.
func (d *Deps) ackPrerequisites(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "migrationID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid migrationID")
		return
	}
	var req struct {
		Acked []string `json:"acked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := d.Repo.SetAckedPrereqs(r.Context(), id, req.Acked); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"acked": req.Acked})
}

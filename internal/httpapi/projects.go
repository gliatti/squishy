package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"gitlab.com/dalibo/squishy/internal/project"
)

// ---- projects ----

type createProjectReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

func (d *Deps) createProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		errJSON(w, http.StatusBadRequest, "name required")
		return
	}
	slug := strings.ToLower(req.Name)
	slug = slugRe.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "p-" + time.Now().Format("20060102150405")
	}
	base := slug
	for i := 0; i < 5; i++ {
		p := &project.Project{Name: req.Name, Slug: slug, Description: req.Description}
		err := d.Repo.CreateProject(r.Context(), p)
		if err == nil {
			createdJSON(w, p)
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			slug = base + "-" + time.Now().Format("150405") + "-" + string(rune('a'+i))
			continue
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	errJSON(w, http.StatusConflict, "could not generate unique slug; try another name")
}

func (d *Deps) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := d.Repo.ListProjects(r.Context())
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"projects": ps})
}

func (d *Deps) getProject(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid projectID")
		return
	}
	p, err := d.Repo.GetProject(r.Context(), id)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, p)
}

func (d *Deps) deleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid projectID")
		return
	}
	if err := d.Repo.DeleteProject(r.Context(), id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type updateProjectReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (d *Deps) updateProject(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid projectID")
		return
	}
	var req updateProjectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		errJSON(w, http.StatusBadRequest, "name required")
		return
	}
	p, err := d.Repo.UpdateProject(r.Context(), id, req.Name, req.Description)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, p)
}

// toString is a tiny helper used by handlers that turn map[string]any param
// blobs into the string-keyed map the connection driver expects.
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	}
	b, _ := json.Marshal(v)
	return string(b)
}

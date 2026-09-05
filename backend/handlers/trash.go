package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type TrashHandler struct{}

func (h *TrashHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/{id}/restore", h.Restore)
	r.Delete("/{id}", h.Purge)

	// Emptying the bin is irreversible, so it is admin-only.
	r.With(middleware.Admin).Delete("/empty", h.Empty)
}

// GET /api/trash
func (h *TrashHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.TrashColumns+`
		  from public.trash t
		 where ($1 = '' or t.kind = $1)
		 order by t.deleted_at desc
		 limit $2`, query(r, "kind", ""), queryInt(r, "limit", 200, 1, 1000))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/trash/:id/restore
//
// restore_trash puts the row and its children back in one transaction, so a
// component returns with its units and comments intact.
func (h *TrashHandler) Restore(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var restored string
	if err := db.GetDB().QueryRow(r.Context(),
		`select public.restore_trash($1, $2)`, id, actor(r)).Scan(&restored); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]any{"restored": true, "id": restored})
}

// DELETE /api/trash/:id — permanently discard one entry.
func (h *TrashHandler) Purge(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	tag, err := db.GetDB().Exec(r.Context(), `delete from public.trash where tid = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, r, db.ErrNotFound)
		return
	}
	_ = logActivity(r, db.GetDB(), "Discarded a deleted record", "×", "#c0655f", "trash", id)
	noContent(w)
}

// DELETE /api/trash/empty — admin only, and the count is recorded.
func (h *TrashHandler) Empty(w http.ResponseWriter, r *http.Request) {
	tag, err := db.GetDB().Exec(r.Context(), `delete from public.trash`)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(),
		"Emptied the trash ("+itoa(int(tag.RowsAffected()))+" records)", "×", "#c0655f", "trash", "")
	ok(w, map[string]int64{"discarded": tag.RowsAffected()})
}

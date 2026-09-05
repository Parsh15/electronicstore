package handlers

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/services"
)

// Backup and restore. Every route here is admin-only, applied by the router,
// because both directions touch the entire store.
type BackupHandler struct{}

func (h *BackupHandler) Routes(r chi.Router) {
	r.Post("/create", h.Create)
	r.Get("/create", h.Create) // a plain link in the settings screen can hit it
	r.Post("/restore", h.Restore)
	r.Get("/history", h.History)
	r.Get("/meta", h.Meta)
	r.Get("/{id}/download", h.Create) // ids are not persisted; a download is a fresh snapshot
}

// POST /api/backup/create — the whole store as one JSON file.
func (h *BackupHandler) Create(w http.ResponseWriter, r *http.Request) {
	snapshot, err := services.Create(r.Context(), actor(r), actorID(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	download(w, services.Filename(), "application/json; charset=utf-8")
	_, _ = w.Write(snapshot)
}

// POST /api/backup/restore
//
// Accepts either the file directly or {"data": {...}}, which is what the
// existing restore dialog posts. Validation happens before the transaction
// opens; the transaction itself is all-or-nothing.
func (h *BackupHandler) Restore(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "The backup file could not be read.")
		return
	}
	if len(raw) == 0 {
		errorJSON(w, http.StatusBadRequest, "No backup file was sent.")
		return
	}

	payload := raw
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Data) > 0 {
		payload = wrapper.Data
	}

	counts, err := services.Restore(r.Context(), payload, actor(r), actorID(r))
	if err != nil {
		// Restore failures are almost always a bad file, and the message says
		// which — so it goes back as a 400 rather than a generic 500.
		plain(w, http.StatusBadRequest, err)
		return
	}
	ok(w, map[string]any{"restored": true, "counts": counts})
}

// GET /api/backup/history
func (h *BackupHandler) History(w http.ResponseWriter, r *http.Request) {
	body, err := services.History(r.Context(), queryInt(r, "limit", 50, 1, 200))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/backup/meta — what a backup would contain, without producing one.
func (h *BackupHandler) Meta(w http.ResponseWriter, r *http.Request) {
	meta, err := services.Meta(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, meta)
}

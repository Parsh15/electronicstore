package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
)

type HealthHandler struct {
	Version string
	Started time.Time
}

// GET /api/health
//
// Deliberately unauthenticated and deliberately quiet: it reports whether the
// service is up, whether the database answers, and whether the schema is in
// place — and nothing about the host, the connection string or the data.
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	status := "ok"
	dbUp := true
	if err := db.GetDB().Ping(ctx); err != nil {
		status = "degraded"
		dbUp = false
	}

	schemaReady := false
	if dbUp {
		schemaReady, _ = db.SchemaReady(ctx)
		if !schemaReady {
			status = "degraded"
		}
	}

	body := map[string]any{
		"status":      status,
		"database":    dbUp,
		"schema":      schemaReady,
		"version":     h.Version,
		"uptime":      int(time.Since(h.Started).Seconds()),
		"time":        time.Now().UTC().Format(time.RFC3339),
		"signedIn":    false,
	}

	// A signed-in caller gets the pool statistics too — useful when the app
	// feels slow and nobody wants to open a shell.
	if u, okUser := middleware.FromContext(r.Context()); okUser && u != nil {
		body["signedIn"] = true
		if u.IsAdmin() {
			body["pool"] = db.Stats()
		}
	}

	code := http.StatusOK
	if status != "ok" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, body)
}

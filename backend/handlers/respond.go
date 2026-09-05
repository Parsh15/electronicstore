// Package handlers holds the HTTP layer. Every handler follows the same four
// steps: read the path/query parameters, decode and validate the body, call the
// database or a service, write the response.
//
// Errors never carry SQL text or stack traces to the client — db.Classify maps
// a Postgres error to a status code and a sentence a person can act on, and the
// detail is logged server-side.
package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

// ------------------------------------------------------------------ responses

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("response encode failed: %v", err)
	}
}

// writeRaw sends JSON the database already produced, avoiding a decode/encode
// round trip on list endpoints.
func writeRaw(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if len(body) == 0 {
		body = []byte("null")
	}
	_, _ = w.Write(body)
}

func ok(w http.ResponseWriter, v any)      { writeJSON(w, http.StatusOK, v) }
func created(w http.ResponseWriter, v any) { writeJSON(w, http.StatusCreated, v) }

func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

func errorJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// fail is the single exit for anything that went wrong below the handler.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := db.Classify(err)
	if code >= 500 {
		log.Printf("error: %s %s — %v", r.Method, r.URL.Path, err)
	}
	errorJSON(w, code, msg)
}

// bad renders a validation problem, keeping the field name so the UI can
// highlight the right input.
func bad(w http.ResponseWriter, p *models.Problem) {
	writeJSON(w, http.StatusBadRequest, p)
}

// plain sends an error from a service that is already user-facing text.
func plain(w http.ResponseWriter, code int, err error) {
	errorJSON(w, code, err.Error())
}

// ------------------------------------------------------------------- requests

// decode reads a JSON body, rejecting unknown fields so a typo in a client
// payload is caught rather than silently ignored.
func decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("This request needs a body.")
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("This request needs a body.")
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errors.New("That request was too large.")
		}
		return errors.New("The request body could not be read.")
	}
	return nil
}

// idParam pulls a UUID path parameter, or writes a 400 and returns false.
func idParam(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	v := chi.URLParam(r, name)
	if !models.IsUUID(v) {
		errorJSON(w, http.StatusBadRequest, "That record id is not valid.")
		return "", false
	}
	return v, true
}

// textParam pulls a non-UUID path parameter (a unit code, for instance).
func textParam(w http.ResponseWriter, r *http.Request, name string, maxLen int) (string, bool) {
	v := strings.TrimSpace(chi.URLParam(r, name))
	if v == "" || len(v) > maxLen {
		errorJSON(w, http.StatusBadRequest, "That identifier is not valid.")
		return "", false
	}
	return v, true
}

func query(r *http.Request, name, def string) string {
	if v := strings.TrimSpace(r.URL.Query().Get(name)); v != "" {
		return v
	}
	return def
}

func queryInt(r *http.Request, name string, def, min, max int) int {
	v, err := strconv.Atoi(query(r, name, ""))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func queryBool(r *http.Request, name string) bool {
	v := strings.ToLower(query(r, name, ""))
	return v == "1" || v == "true" || v == "yes"
}

// actor is the display name written into activity rows.
func actor(r *http.Request) string { return middleware.MustUser(r).Actor() }

func actorID(r *http.Request) string { return middleware.MustUser(r).ID }

// download sets the headers that make a browser save rather than display.
func download(w http.ResponseWriter, filename, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
}

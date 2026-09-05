package handlers

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

// Helpers shared by more than one handler file: soft delete, comments, CSV
// streaming, and the small parameter conversions that keep every query
// parameterised.

// rawJSON lets a []byte of database-produced JSON be nested inside a Go map
// without being re-encoded as a base64 string.
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

func ptr(s string) *string { return &s }

// uuidOrNil converts an optional id into a bindable value; an empty or invalid
// string becomes SQL NULL rather than a type error.
func uuidOrNil(s *string) any {
	if s == nil || *s == "" || !models.IsUUID(*s) {
		return nil
	}
	return *s
}

// uuidOrEmpty is for filters written as `$1 = '' or col = $1::uuid`.
func uuidOrEmpty(s string) string {
	if models.IsUUID(s) {
		return s
	}
	return ""
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }

// softDelete moves a row into the trash via the stored function. labelCol names
// the column used as the human-readable label on the trash entry.
func softDelete(w http.ResponseWriter, r *http.Request, table, labelCol string) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}

	var tid string
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var label string
		// labelCol and table are compile-time constants at every call site —
		// never user input — so this format is safe.
		if err := tx.QueryRow(r.Context(),
			fmt.Sprintf(`select coalesce(%s::text, '') from public.%s where id = $1`, labelCol, table),
			id).Scan(&label); err != nil {
			return err
		}
		return tx.QueryRow(r.Context(),
			`select public.soft_delete($1, $2, $3, $4)`, table, id, label, actor(r)).Scan(&tid)
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]any{"deleted": true, "tid": tid})
}

// addComment writes a note against whichever entity owns it. ownerCol is one of
// the five owner columns on public.comments and is always a literal here.
func addComment(w http.ResponseWriter, r *http.Request, ownerCol, ownerID string) {
	var req models.CommentRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}
	if req.Author == "" {
		req.Author = actor(r)
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), fmt.Sprintf(`
		insert into public.comments (%s, author, body, tag)
		values ($1, $2, $3, $4)
		returning id, author, body as text, tag,
		          extract(epoch from created_at) * 1000 as ts`, ownerCol),
		ownerID, req.Author, req.Body, req.Tag)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusCreated, body)
}

// writeCSVRows streams rows straight to the response writer — no temp files,
// no buffering the whole export in memory.
func writeCSVRows(w http.ResponseWriter, cols []string, rows [][]any) {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	_ = cw.Write(cols)
	for _, row := range rows {
		out := make([]string, len(row))
		for i, v := range row {
			out[i] = csvCell(v)
		}
		if err := cw.Write(out); err != nil {
			return
		}
	}
}

func csvCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int, int32, int64:
		return fmt.Sprintf("%d", t)
	case time.Time:
		return t.Format("2006-01-02 15:04")
	case []byte:
		return string(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		s := string(b)
		if len(s) > 1 && s[0] == '"' && s[len(s)-1] == '"' {
			return s[1 : len(s)-1]
		}
		return s
	}
}

// logActivity appends one audit line. Callers that already run in a transaction
// pass the tx; the rest pass db.GetDB().
func logActivity(r *http.Request, q db.Querier, body, glyph, color, entityType, entityID string) error {
	_, err := q.Exec(r.Context(), `
		insert into public.activity (body, glyph, color, actor, actor_id, entity_type, entity_id)
		values ($1, $2, $3, $4, $5, $6, $7)`,
		body, glyph, color, actor(r), uuidOrNil(ptr(actorID(r))), entityType, entityID)
	return err
}

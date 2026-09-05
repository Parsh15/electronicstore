package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type ActivityHandler struct{}

func (h *ActivityHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/export", h.Export)
}

// GET /api/activity — the feed behind the dashboard and the audit screen.
func (h *ActivityHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.ActivityColumns+`
		  from public.activity a
		 where ($1 = '' or a.entity_type = $1)
		   and ($2 = '' or a.entity_id = $2)
		   and ($3 = '' or a.actor_id = $3::uuid)
		   and ($4 = '' or a.created_at >= $4::date)
		   and ($5 = '' or a.created_at < ($5::date + 1))
		 order by a.created_at desc
		 limit $6 offset $7`,
		query(r, "entityType", ""), query(r, "entityId", ""),
		uuidOrEmpty(query(r, "userId", "")), query(r, "from", ""), query(r, "to", ""),
		queryInt(r, "limit", 100, 1, 1000), queryInt(r, "offset", 0, 0, 100_000))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/activity — for the few client-side events worth recording, such as
// a label print run. The actor is taken from the session, never the body.
func (h *ActivityHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.ActivityRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		insert into public.activity (body, glyph, color, actor, actor_id, entity_type, entity_id)
		values ($1, $2, $3, $4, $5, $6, $7)
		returning id, body as text, glyph, color, actor,
		          entity_type as "entityType", entity_id as "entityId",
		          extract(epoch from created_at) * 1000 as ts`,
		req.Body, req.Glyph, req.Color, actor(r), uuidOrNil(ptr(actorID(r))),
		req.EntityType, req.EntityID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusCreated, body)
}

// GET /api/activity/export — CSV of the audit trail, streamed.
func (h *ActivityHandler) Export(w http.ResponseWriter, r *http.Request) {
	cols, rows, err := db.Rows(r.Context(), db.GetDB(), `
		select a.created_at, a.actor, a.body, a.entity_type, a.entity_id
		  from public.activity a
		 where ($1 = '' or a.created_at >= $1::date)
		   and ($2 = '' or a.created_at < ($2::date + 1))
		 order by a.created_at desc
		 limit 20000`, query(r, "from", ""), query(r, "to", ""))
	if err != nil {
		fail(w, r, err)
		return
	}
	download(w, "activity.csv", "text/csv; charset=utf-8")
	writeCSVRows(w, cols, rows)
}

// ---------------------------------------------------------------- voice log

type VoiceHandler struct{}

func (h *VoiceHandler) Routes(r chi.Router) {
	r.Post("/log", h.Log)
	r.Get("/log", h.List)
}

// POST /api/voice/log
//
// Recognition stays in the browser with the Web Speech API; this only records
// what was said and whether it worked, so the command grammar can be improved.
func (h *VoiceHandler) Log(w http.ResponseWriter, r *http.Request) {
	var req models.VoiceLogRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	if _, err := db.GetDB().Exec(r.Context(), `
		insert into public.voice_log (user_id, command, action, success)
		values ($1, $2, $3, $4)`,
		uuidOrNil(ptr(actorID(r))), req.Command, nullString(req.Action), req.Success); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]bool{"logged": true})
}

// GET /api/voice/log
func (h *VoiceHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select v.id, v.command, v.action, v.success,
		       coalesce(p.name, '') as "by",
		       extract(epoch from v.created_at) * 1000 as ts
		  from public.voice_log v
		  left join public.profiles p on p.id = v.user_id
		 order by v.created_at desc
		 limit $1`, queryInt(r, "limit", 100, 1, 1000))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// ------------------------------------------------------------------- search

type SearchHandler struct{}

// GET /api/search?q=resistor
//
// One database round trip returns results grouped by kind — components,
// projects, suppliers, boxes and units — which is what the ⌘K palette expects.
func (h *SearchHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := query(r, "q", "")
	if len(q) < 1 {
		ok(w, map[string]any{
			"query": "", "components": []any{}, "projects": []any{},
			"suppliers": []any{}, "boxes": []any{}, "units": []any{},
		})
		return
	}
	if len(q) > 100 {
		q = q[:100]
	}

	body, err := db.ScalarJSON(r.Context(), db.GetDB(),
		`select public.global_search($1, $2)::text`, q, queryInt(r, "limit", 8, 1, 50))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

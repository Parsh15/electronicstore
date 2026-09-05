package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type EventHandler struct{}

func (h *EventHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)

	r.Route("/{id}", func(er chi.Router) {
		er.Get("/", h.Get)
		er.Put("/", h.Update)
		er.Delete("/", h.Delete)
	})
}

// GET /api/events — with the funds attached to each, since the funding screen
// shows them together.
func (h *EventHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.EventColumns+`,
		       (select count(*) from public.funds f where f.event_id = e.id) as "fundCount",
		       case when e.event_date is null then null
		            else e.event_date - current_date end as "daysLeft"
		  from public.events e
		 where ($1 = '' or e.name ilike '%'||$1||'%' or e.org ilike '%'||$1||'%')
		   and ($2 = '' or e.type = $2)
		 order by e.event_date nulls last, e.name`,
		query(r, "q", ""), query(r, "type", ""))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/events/:id
func (h *EventHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select `+models.EventColumns+`,
		       coalesce((select json_agg(t) from (
		         select f.id, f.code, f.name, f.status, f.currency,
		                f.requested, f.approved, f.received
		           from public.funds f where f.event_id = e.id
		          order by f.created_at desc) t), '[]'::json) as funds
		  from public.events e where e.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/events
func (h *EventHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.EventRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		insert into public.events (name, org, type, event_date, venue, notes)
		values ($1, $2, $3, nullif($4,'')::date, $5, $6)
		returning id, name, org, type, coalesce(event_date::text, '') as date, venue, notes`,
		req.Name, req.Org, req.Type, req.Date, req.Venue, req.Notes)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(), `Added event "`+req.Name+`"`, "＋", "#5faa87", "event", "")
	writeRaw(w, http.StatusCreated, body)
}

// PUT /api/events/:id
func (h *EventHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.EventRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		update public.events e set
		  name = $2, org = $3, type = $4, event_date = nullif($5,'')::date,
		  venue = $6, notes = $7
		 where e.id = $1
		returning e.id, e.name, e.org, e.type,
		          coalesce(e.event_date::text, '') as date, e.venue, e.notes`,
		id, req.Name, req.Org, req.Type, req.Date, req.Venue, req.Notes)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// DELETE /api/events/:id — funds keep their record and lose the event link.
func (h *EventHandler) Delete(w http.ResponseWriter, r *http.Request) {
	softDelete(w, r, "events", "name")
}

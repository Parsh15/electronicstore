package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type FundHandler struct{}

func (h *FundHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/totals", h.Totals)
	r.Get("/pipeline", h.Pipeline)

	r.Route("/{id}", func(fr chi.Router) {
		fr.Get("/", h.Get)
		fr.Put("/", h.Update)
		fr.Delete("/", h.Delete)
		fr.Post("/advance", h.Advance)
		fr.Get("/history", h.History)
		fr.Post("/comments", h.AddComment)
	})
}

// GET /api/funds
func (h *FundHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.FundColumns+`,
		       e.name as "eventName",
		       coalesce((select json_agg(fp.project_id) from public.fund_projects fp
		                  where fp.fund_id = f.id), '[]'::json) as "projectIds",
		       (f.deadline is not null and f.deadline < current_date
		        and f.status in ('Draft','Applied','Under Review')) as overdue
		  from public.funds f
		  left join public.events e on e.id = f.event_id
		 where ($1 = '' or f.name ilike '%'||$1||'%' or f.provider ilike '%'||$1||'%'
		        or f.code ilike '%'||$1||'%')
		   and ($2 = '' or f.status = $2)
		   and ($3 = '' or f.currency = $3)
		   and ($4 = '' or f.event_id = $4::uuid)
		 order by f.created_at desc`,
		query(r, "q", ""), query(r, "status", ""), query(r, "currency", ""),
		uuidOrEmpty(query(r, "eventId", "")))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/funds/:id — the fund with its linked projects, parts and history.
func (h *FundHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select `+models.FundColumns+`,
		       e.name as "eventName",
		       coalesce((select json_agg(fp.project_id) from public.fund_projects fp
		                  where fp.fund_id = f.id), '[]'::json) as "projectIds",
		       coalesce((select json_agg(t) from (
		         select p.id, p.code, p.name, p.status
		           from public.fund_projects fp
		           join public.projects p on p.id = fp.project_id
		          where fp.fund_id = f.id) t), '[]'::json) as projects,
		       coalesce((select json_agg(t) from (
		         select fa.component_id as id, fa.qty, c.code, c.name, c.price
		           from public.fund_parts fa
		           join public.components c on c.id = fa.component_id
		          where fa.fund_id = f.id) t), '[]'::json) as parts,
		       coalesce((select json_agg(t) from (
		         select h.id, h.status, h.note, h.created_by as "by",
		                extract(epoch from h.created_at) * 1000 as ts
		           from public.fund_history h where h.fund_id = f.id
		          order by h.created_at) t), '[]'::json) as history,
		       coalesce((select json_agg(t) from (
		         select m.id, m.author, m.body as text, m.tag,
		                extract(epoch from m.created_at) * 1000 as ts
		           from public.comments m where m.fund_id = f.id
		          order by m.created_at desc) t), '[]'::json) as comments
		  from public.funds f
		  left join public.events e on e.id = f.event_id
		 where f.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/funds — the fund, its project links, its parts and its first
// history entry all land together.
func (h *FundHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.FundRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	var body []byte
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(r.Context(), `
			insert into public.funds (name, provider, kind, currency, event_id, requested,
			                          approved, received, applied_on, deadline, status,
			                          contact, ref, notes, docs)
			values ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9,'')::date, nullif($10,'')::date,
			        $11, $12, $13, $14, $15)
			returning id`,
			req.Name, req.Provider, req.Kind, req.Currency, uuidOrNil(req.EventID),
			req.Requested, req.Approved, req.Received, req.AppliedOn, req.Deadline,
			req.Status, req.Contact, req.Ref, req.Notes, req.Docs).Scan(&id); err != nil {
			return err
		}

		if err := writeFundLinks(r, tx, id, req.ProjectIDs, req.Parts); err != nil {
			return err
		}

		if _, err := tx.Exec(r.Context(), `
			insert into public.fund_history (fund_id, status, note, created_by)
			values ($1, $2, 'Fund record created', $3)`, id, req.Status, actor(r)); err != nil {
			return err
		}
		if err := logActivity(r, tx, `Added funding "`+req.Name+`"`, "₹", "#c8a06c", "fund", id); err != nil {
			return err
		}

		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.FundColumns+` from public.funds f where f.id = $1`, id)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusCreated, body)
}

// PUT /api/funds/:id
func (h *FundHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.FundRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	var body []byte
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var before string
		if err := tx.QueryRow(r.Context(),
			`select status from public.funds where id = $1 for update`, id).Scan(&before); err != nil {
			return err
		}

		if _, err := tx.Exec(r.Context(), `
			update public.funds set
			  name = $2, provider = $3, kind = $4, currency = $5, event_id = $6,
			  requested = $7, approved = $8, received = $9,
			  applied_on = nullif($10,'')::date, deadline = nullif($11,'')::date,
			  status = $12, contact = $13, ref = $14, notes = $15, docs = $16
			 where id = $1`,
			id, req.Name, req.Provider, req.Kind, req.Currency, uuidOrNil(req.EventID),
			req.Requested, req.Approved, req.Received, req.AppliedOn, req.Deadline,
			req.Status, req.Contact, req.Ref, req.Notes, req.Docs); err != nil {
			return err
		}

		if err := writeFundLinks(r, tx, id, req.ProjectIDs, req.Parts); err != nil {
			return err
		}

		// A status changed through the editor still earns a history line, so
		// the audit trail does not depend on which button was used.
		if before != req.Status {
			if _, err := tx.Exec(r.Context(), `
				insert into public.fund_history (fund_id, status, note, created_by)
				values ($1, $2, 'Status changed while editing', $3)`,
				id, req.Status, actor(r)); err != nil {
				return err
			}
		}

		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.FundColumns+` from public.funds f where f.id = $1`, id)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// writeFundLinks replaces the project and part links. Called with the whole
// list, so removals work as well as additions.
func writeFundLinks(r *http.Request, tx pgx.Tx, fundID string,
	projectIDs []string, parts []models.BOMLine) error {

	if projectIDs != nil {
		if _, err := tx.Exec(r.Context(),
			`delete from public.fund_projects where fund_id = $1`, fundID); err != nil {
			return err
		}
		for _, pid := range projectIDs {
			if _, err := tx.Exec(r.Context(), `
				insert into public.fund_projects (fund_id, project_id) values ($1, $2)
				on conflict do nothing`, fundID, pid); err != nil {
				return err
			}
		}
	}
	if parts != nil {
		if _, err := tx.Exec(r.Context(),
			`delete from public.fund_parts where fund_id = $1`, fundID); err != nil {
			return err
		}
		for _, part := range parts {
			if _, err := tx.Exec(r.Context(), `
				insert into public.fund_parts (fund_id, component_id, qty) values ($1, $2, $3)
				on conflict (fund_id, component_id) do update set qty = excluded.qty`,
				fundID, part.ComponentID, part.Qty); err != nil {
				return err
			}
		}
	}
	return nil
}

// DELETE /api/funds/:id
func (h *FundHandler) Delete(w http.ResponseWriter, r *http.Request) {
	softDelete(w, r, "funds", "name")
}

// POST /api/funds/:id/advance — status change and audit line, atomically.
func (h *FundHandler) Advance(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.AdvanceFundRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select `+models.FundColumns+`
		  from (select (public.advance_fund($1, $2, $3, $4)).*) f`,
		id, req.Status, req.Note, actor(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/funds/:id/history
func (h *FundHandler) History(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select h.id, h.status, h.note, h.created_by as "by",
		       extract(epoch from h.created_at) * 1000 as ts
		  from public.fund_history h where h.fund_id = $1
		 order by h.created_at`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/funds/totals
func (h *FundHandler) Totals(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select currency, status, records, requested, approved, received
		  from public.v_fund_totals
		 order by currency, status`)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/funds/pipeline
func (h *FundHandler) Pipeline(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select id, code, name, provider, kind, currency, status,
		       requested, approved, received,
		       coalesce(deadline::text, '') as deadline,
		       event_name as "eventName", overdue, days_left as "daysLeft",
		       linked_projects as "linkedProjects"
		  from public.v_fund_pipeline
		 order by deadline asc nulls last`)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/funds/:id/comments
func (h *FundHandler) AddComment(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	addComment(w, r, "fund_id", id)
}

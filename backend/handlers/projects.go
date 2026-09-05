package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type ProjectHandler struct{}

func (h *ProjectHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)

	r.Route("/{id}", func(pr chi.Router) {
		pr.Get("/", h.Get)
		pr.Put("/", h.Update)
		pr.Delete("/", h.Delete)
		pr.Post("/complete", h.Complete)
		pr.Post("/reserve-units", h.ReserveUnits)
		pr.Get("/bom", h.GetBOM)
		pr.Post("/bom", h.AddBOM)
		pr.Put("/bom/{partId}", h.UpdateBOMLine)
		pr.Delete("/bom/{partId}", h.DeleteBOMLine)
		pr.Get("/comments", h.Comments)
		pr.Post("/comments", h.AddComment)
	})
}

// GET /api/projects — includes the cost rollup so the list can show it without
// a second request per row.
func (h *ProjectHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.ProjectColumns+`,
		       coalesce(vc.line_items, 0)  as "lineItems",
		       coalesce(vc.bom_cost, 0)    as "bomCost",
		       coalesce(vc.short_lines, 0) as "shortLines",
		       coalesce(vc.buildable, true) as buildable
		  from public.projects p
		  left join public.v_project_cost vc on vc.project_id = p.id
		 where ($1 = '' or p.name ilike '%'||$1||'%' or p.code ilike '%'||$1||'%'
		        or p.description ilike '%'||$1||'%')
		   and ($2 = '' or p.status = $2)
		 order by p.created_at desc
		 limit $3 offset $4`,
		query(r, "q", ""), query(r, "status", ""),
		queryInt(r, "limit", 500, 1, 2000), queryInt(r, "offset", 0, 0, 100_000))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/projects/:id — the project with its BOM and comments attached.
func (h *ProjectHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select `+models.ProjectColumns+`,
		       coalesce((select json_agg(t) from (
		         select pp.id as "partId", pp.component_id as id, pp.qty, pp.status,
		                pp.unit_ids as "unitIds", c.code, c.name, c.category, c.location,
		                c.quantity as "onHand", c.price,
		                (c.quantity >= pp.qty) as coverable
		           from public.project_parts pp
		           join public.components c on c.id = pp.component_id
		          where pp.project_id = p.id
		          order by c.name) t), '[]'::json) as parts,
		       coalesce((select json_agg(t) from (
		         select m.id, m.author, m.body as text, m.tag,
		                extract(epoch from m.created_at) * 1000 as ts
		           from public.comments m where m.project_id = p.id
		          order by m.created_at desc) t), '[]'::json) as comments,
		       coalesce((select json_agg(t) from (
		         select f.id, f.code, f.name, f.status, f.currency, f.approved
		           from public.fund_projects fp
		           join public.funds f on f.id = fp.fund_id
		          where fp.project_id = p.id) t), '[]'::json) as funds
		  from public.projects p
		 where p.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/projects
func (h *ProjectHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		models.ProjectRequest
		Parts []models.BOMLine `json:"parts"`
	}
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.ProjectRequest.Validate(); p != nil {
		bad(w, p)
		return
	}
	for i := range req.Parts {
		if p := req.Parts[i].Validate(); p != nil {
			bad(w, p)
			return
		}
	}

	var body []byte
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(r.Context(), `
			insert into public.projects (name, description, detail, file_name, status)
			values ($1, $2, $3, $4, $5) returning id`,
			req.Name, req.Description, req.Detail, req.FileName, req.Status).Scan(&id); err != nil {
			return err
		}
		for _, part := range req.Parts {
			if _, err := tx.Exec(r.Context(), `
				insert into public.project_parts (project_id, component_id, qty, status)
				values ($1, $2, $3, $4)
				on conflict (project_id, component_id) do update set qty = excluded.qty`,
				id, part.ComponentID, part.Qty, part.Status); err != nil {
				return err
			}
		}
		if err := logActivity(r, tx, `Created project "`+req.Name+`"`, "＋", "#5faa87", "project", id); err != nil {
			return err
		}
		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.ProjectColumns+` from public.projects p where p.id = $1`, id)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusCreated, body)
}

// PUT /api/projects/:id
func (h *ProjectHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.ProjectRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		update public.projects p set
		  name = $2, description = $3, detail = $4, file_name = $5, status = $6,
		  started_at = case when $6 = 'active' then coalesce(p.started_at, now()) else p.started_at end,
		  completed_at = case when $6 = 'complete' then coalesce(p.completed_at, now()) else null end
		 where p.id = $1
		returning `+models.ProjectColumns,
		id, req.Name, req.Description, req.Detail, req.FileName, req.Status)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(), `Edited project "`+req.Name+`"`, "✎", "#8da2c8", "project", id)
	writeRaw(w, http.StatusOK, body)
}

// DELETE /api/projects/:id — soft delete; the BOM and comments go with it.
func (h *ProjectHandler) Delete(w http.ResponseWriter, r *http.Request) {
	softDelete(w, r, "projects", "name")
}

// POST /api/projects/:id/complete
//
// complete_project deducts every BOM quantity, retires the reserved units,
// closes the project and reports what fell to low stock — one transaction, so a
// half-consumed build is not a state the store can end up in.
func (h *ProjectHandler) Complete(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.ScalarJSON(r.Context(), db.GetDB(),
		`select public.complete_project($1, $2)::text`, id, actor(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/projects/:id/reserve-units
//
// Claims free units for every BOM line. If any line cannot be covered the
// function raises, the transaction rolls back, and nothing is half-reserved.
func (h *ProjectHandler) ReserveUnits(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var reserved int
	if err := db.GetDB().QueryRow(r.Context(),
		`select public.reserve_project_units($1, $2)`, id, actor(r)).Scan(&reserved); err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]int{"unitsReserved": reserved})
}

// GET /api/projects/:id/bom
func (h *ProjectHandler) GetBOM(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select part_id as "partId", component_id as "componentId", component_code as code,
		       component_name as name, category, location, qty, part_status as status,
		       on_hand as "onHand", short_by as "shortBy", line_cost as "lineCost", coverable
		  from public.v_project_bom
		 where project_id = $1
		 order by component_name`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/projects/:id/bom — add lines, or replace the whole BOM.
func (h *ProjectHandler) AddBOM(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.BOMRequest
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
		if req.Replace {
			if _, err := tx.Exec(r.Context(),
				`delete from public.project_parts where project_id = $1`, id); err != nil {
				return err
			}
		}
		for _, part := range req.Parts {
			if _, err := tx.Exec(r.Context(), `
				insert into public.project_parts (project_id, component_id, qty, status)
				values ($1, $2, $3, $4)
				on conflict (project_id, component_id)
				do update set qty = excluded.qty, status = excluded.status`,
				id, part.ComponentID, part.Qty, part.Status); err != nil {
				return err
			}
		}
		var err error
		body, err = db.QueryJSON(r.Context(), tx, `
			select part_id as "partId", component_id as "componentId", component_code as code,
			       component_name as name, qty, part_status as status, on_hand as "onHand",
			       line_cost as "lineCost", coverable
			  from public.v_project_bom where project_id = $1 order by component_name`, id)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// PUT /api/projects/:id/bom/:partId
func (h *ProjectHandler) UpdateBOMLine(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	partID, valid := idParam(w, r, "partId")
	if !valid {
		return
	}
	var req models.BOMUpdateRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		update public.project_parts pp
		   set qty = coalesce($3, pp.qty), status = coalesce($4, pp.status)
		 where pp.id = $2 and pp.project_id = $1
		returning pp.id as "partId", pp.component_id as "componentId", pp.qty, pp.status`,
		id, partID, req.Qty, req.Status)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// DELETE /api/projects/:id/bom/:partId
func (h *ProjectHandler) DeleteBOMLine(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	partID, valid := idParam(w, r, "partId")
	if !valid {
		return
	}
	tag, err := db.GetDB().Exec(r.Context(),
		`delete from public.project_parts where id = $1 and project_id = $2`, partID, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, r, db.ErrNotFound)
		return
	}
	noContent(w)
}

// GET /api/projects/:id/comments
func (h *ProjectHandler) Comments(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select m.id, m.author, m.body as text, m.tag,
		       extract(epoch from m.created_at) * 1000 as ts
		  from public.comments m where m.project_id = $1
		 order by m.created_at desc`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/projects/:id/comments
func (h *ProjectHandler) AddComment(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	addComment(w, r, "project_id", id)
}

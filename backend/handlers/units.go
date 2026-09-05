package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type UnitHandler struct{}

func (h *UnitHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/bulk-status", h.BulkStatus)

	r.Route("/{id}", func(ur chi.Router) {
		ur.Get("/", h.Get)
		ur.Put("/", h.Update)
		ur.Put("/status", h.SetStatus)
		ur.Post("/reserve", h.Reserve)
		ur.Get("/history", h.History)
	})
}

// GET /api/units — the units table, filterable by component, status and project.
func (h *UnitHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.UnitColumns+`,
		       c.name as "componentName", c.code as "componentCode",
		       p.name as "projectName"
		  from public.component_units u
		  join public.components c on c.id = u.component_id
		  left join public.projects p on p.id = u.project_id
		 where ($1 = '' or u.component_id = $1::uuid)
		   and ($2 = '' or u.status = $2)
		   and ($3 = '' or u.project_id = $3::uuid)
		   and ($4 = '' or u.unit_id ilike '%'||$4||'%' or c.name ilike '%'||$4||'%')
		   and (not $5 or u.faulty)
		 order by u.unit_id
		 limit $6 offset $7`,
		uuidOrEmpty(query(r, "componentId", "")), query(r, "status", ""),
		uuidOrEmpty(query(r, "projectId", "")), query(r, "q", ""), queryBool(r, "faulty"),
		queryInt(r, "limit", 2000, 1, 5000), queryInt(r, "offset", 0, 0, 1_000_000))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/units/:id
func (h *UnitHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select `+models.UnitColumns+`,
		       c.name as "componentName", c.code as "componentCode",
		       p.name as "projectName",
		       coalesce((select json_agg(t) from (
		         select m.id, m.author, m.body as text, m.tag,
		                extract(epoch from m.created_at) * 1000 as ts
		           from public.comments m where m.unit_id = u.unit_id
		          order by m.created_at desc) t), '[]'::json) as comments
		  from public.component_units u
		  join public.components c on c.id = u.component_id
		  left join public.projects p on p.id = u.project_id
		 where u.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// PUT /api/units/:id — rename or re-batch a unit. Status changes go through
// the status endpoint, which has its own rules.
func (h *UnitHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.UnitUpdateRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		update public.component_units u set
		  unit_id   = coalesce($2, u.unit_id),
		  batch     = coalesce($3, u.batch),
		  batch_qty = coalesce($4, u.batch_qty)
		 where u.id = $1
		returning `+models.UnitColumns,
		id, req.UnitID, req.Batch, req.BatchQty)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// PUT /api/units/:id/status
//
// set_unit_status keeps status, the faulty flag and the project link coherent,
// and writes the audit line, so this handler only translates the request.
func (h *UnitHandler) SetStatus(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.UnitStatusRequest
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
		var unitCode string
		if err := tx.QueryRow(r.Context(),
			`select unit_id from public.component_units where id = $1`, id).Scan(&unitCode); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(),
			`select public.set_unit_status($1, $2, $3, $4)`,
			unitCode, req.Status, uuidOrNil(req.ProjectID), actor(r)); err != nil {
			return err
		}
		if req.Note != "" {
			tag := "General"
			if req.Status == "faulty" {
				tag = "Faulty Note"
			}
			if _, err := tx.Exec(r.Context(), `
				insert into public.comments (unit_id, author, body, tag)
				values ($1, $2, $3, $4)`, unitCode, actor(r), req.Note, tag); err != nil {
				return err
			}
		}
		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.UnitColumns+` from public.component_units u where u.id = $1`, id)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/units/:id/reserve — shorthand for moving one unit onto a project.
func (h *UnitHandler) Reserve(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req struct {
		ProjectID string `json:"projectId"`
	}
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := models.ValidUUID("projectId", req.ProjectID); p != nil {
		bad(w, p)
		return
	}

	var body []byte
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var unitCode, status string
		if err := tx.QueryRow(r.Context(),
			`select unit_id, status from public.component_units where id = $1 for update`,
			id).Scan(&unitCode, &status); err != nil {
			return err
		}
		if status != "stock" {
			return models.Invalid("status", "That unit is not in stock, so it cannot be reserved.")
		}
		if _, err := tx.Exec(r.Context(),
			`select public.set_unit_status($1, 'reserved', $2, $3)`,
			unitCode, req.ProjectID, actor(r)); err != nil {
			return err
		}
		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.UnitColumns+` from public.component_units u where u.id = $1`, id)
		return err
	})
	if p, isProblem := err.(*models.Problem); isProblem {
		bad(w, p)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/units/bulk-status
//
// All or nothing: if one unit in the selection cannot move, none of them do.
func (h *UnitHandler) BulkStatus(w http.ResponseWriter, r *http.Request) {
	var req models.BulkStatusRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	changed := 0
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		for _, id := range req.UnitIDs {
			var unitCode string
			// accept either the row id or the printed unit code
			q := `select unit_id from public.component_units where unit_id = $1`
			if models.IsUUID(id) {
				q = `select unit_id from public.component_units where id = $1::uuid`
			}
			if err := tx.QueryRow(r.Context(), q, id).Scan(&unitCode); err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(),
				`select public.set_unit_status($1, $2, $3, $4)`,
				unitCode, req.Status, uuidOrNil(req.ProjectID), actor(r)); err != nil {
				return err
			}
			changed++
		}
		return logActivity(r, tx,
			itoa(changed)+" unit(s) → "+req.Status, "⇉", "#8da2c8", "unit", "")
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]int{"changed": changed})
}

// GET /api/units/:id/history
func (h *UnitHandler) History(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(),
		`select `+models.ActivityColumns+`
		   from public.activity a
		  where a.entity_type = 'unit' and a.entity_id = $1
		  order by a.created_at desc limit $2`, id, queryInt(r, "limit", 100, 1, 500))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

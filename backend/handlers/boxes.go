package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type BoxHandler struct{}

func (h *BoxHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Put("/optimize", h.Optimize)

	r.Route("/{id}", func(br chi.Router) {
		br.Get("/", h.Get)
		br.Put("/", h.Update)
		br.Delete("/", h.Delete)
		br.Get("/contents", h.Contents)
		br.Post("/assign", h.Assign)
		br.Post("/comments", h.AddComment)
	})
}

// GET /api/boxes — with fill levels from v_box_fill.
func (h *BoxHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.BoxColumns+`,
		       coalesce(v.used, 0)     as used,
		       coalesce(v.free, b.capacity) as free,
		       coalesce(v.pct_full, 0) as "pctFull",
		       coalesce(v.distinct_components, 0) as "distinctComponents"
		  from public.boxes b
		  left join public.v_box_fill v on v.id = b.id
		 where ($1 = '' or b.name ilike '%'||$1||'%' or b.code ilike '%'||$1||'%'
		        or b.location ilike '%'||$1||'%')
		 order by b.code`, query(r, "q", ""))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/boxes/:id
func (h *BoxHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select `+models.BoxColumns+`,
		       coalesce(v.used, 0) as used, coalesce(v.free, b.capacity) as free,
		       coalesce(v.pct_full, 0) as "pctFull",
		       coalesce((select json_agg(t) from (
		         select k.component_id as "componentId", k.qty,
		                c.code, c.name, c.category, c.location
		           from public.box_contents k
		           join public.components c on c.id = k.component_id
		          where k.box_id = b.id order by c.name) t), '[]'::json) as contents,
		       coalesce((select json_agg(t) from (
		         select m.id, m.author, m.body as text, m.tag,
		                extract(epoch from m.created_at) * 1000 as ts
		           from public.comments m where m.box_id = b.id
		          order by m.created_at desc) t), '[]'::json) as comments
		  from public.boxes b
		  left join public.v_box_fill v on v.id = b.id
		 where b.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/boxes
func (h *BoxHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.BoxRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		insert into public.boxes (code, name, location, description, capacity, image_name)
		values (public.next_code((select box_prefix from public.settings where id = 1),
		                         'public.box_code_seq'), $1, $2, $3, $4, $5)
		returning id, code, name, location, description, capacity,
		          image_name as "imageName",
		          extract(epoch from created_at) * 1000 as created`,
		req.Name, req.Location, req.Description, req.Capacity, req.ImageName)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(), `Added box "`+req.Name+`"`, "＋", "#5faa87", "box", "")
	writeRaw(w, http.StatusCreated, body)
}

// PUT /api/boxes/:id
func (h *BoxHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.BoxRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		update public.boxes b set
		  name = $2, location = $3, description = $4, capacity = $5, image_name = $6
		 where b.id = $1
		returning b.id, b.code, b.name, b.location, b.description, b.capacity,
		          b.image_name as "imageName"`,
		id, req.Name, req.Location, req.Description, req.Capacity, req.ImageName)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// DELETE /api/boxes/:id
func (h *BoxHandler) Delete(w http.ResponseWriter, r *http.Request) {
	softDelete(w, r, "boxes", "name")
}

// GET /api/boxes/:id/contents
func (h *BoxHandler) Contents(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select k.component_id as "componentId", k.qty, c.code, c.name,
		       c.category, c.location, c.quantity as "totalQuantity"
		  from public.box_contents k
		  join public.components c on c.id = k.component_id
		 where k.box_id = $1
		 order by c.name`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/boxes/:id/assign — put a component in a box, or take it out.
//
// The capacity check and the write share a transaction with FOR UPDATE on the
// box row, so two people assigning at once cannot both fit into the last slot.
func (h *BoxHandler) Assign(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.BoxAssignRequest
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
		if req.Remove {
			if _, err := tx.Exec(r.Context(),
				`delete from public.box_contents where box_id = $1 and component_id = $2`,
				id, req.ComponentID); err != nil {
				return err
			}
		} else {
			var capacity, used int
			if err := tx.QueryRow(r.Context(), `
				select b.capacity, coalesce((select sum(k.qty) from public.box_contents k
				                              where k.box_id = b.id), 0)
				  from public.boxes b where b.id = $1 for update`, id).
				Scan(&capacity, &used); err != nil {
				return err
			}
			if used+req.Qty > capacity {
				return models.Invalid("qty",
					"That box only has room for "+itoa(capacity-used)+" more.")
			}
			if _, err := tx.Exec(r.Context(), `
				insert into public.box_contents (box_id, component_id, qty)
				values ($1, $2, $3)
				on conflict (box_id, component_id) do update set qty = excluded.qty`,
				id, req.ComponentID, req.Qty); err != nil {
				return err
			}
		}
		var err error
		body, err = db.QueryJSON(r.Context(), tx, `
			select k.component_id as "componentId", k.qty, c.code, c.name
			  from public.box_contents k
			  join public.components c on c.id = k.component_id
			 where k.box_id = $1 order by c.name`, id)
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

// PUT /api/boxes/optimize
//
// Consolidates the store: every component that is not yet in a box is placed in
// one with room, preferring a box that already holds its category. Runs as one
// transaction and reports what moved, so it can be reviewed after the fact.
func (h *BoxHandler) Optimize(w http.ResponseWriter, r *http.Request) {
	type move struct {
		Component string `json:"component"`
		Box       string `json:"box"`
		Qty       int    `json:"qty"`
	}
	var moves []move

	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			select c.id, c.name, c.category, c.quantity
			  from public.components c
			 where c.quantity > 0
			   and not exists (select 1 from public.box_contents k where k.component_id = c.id)
			 order by c.category, c.name`)
		if err != nil {
			return err
		}
		type pending struct {
			id, name, category string
			qty                int
		}
		var todo []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.name, &p.category, &p.qty); err != nil {
				rows.Close()
				return err
			}
			todo = append(todo, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, p := range todo {
			var boxID, boxCode string
			err := tx.QueryRow(r.Context(), `
				select b.id, b.code
				  from public.boxes b
				  left join public.box_contents k on k.box_id = b.id
				  left join public.components c   on c.id = k.component_id
				 group by b.id
				having b.capacity - coalesce(sum(k.qty), 0) >= $1
				 order by (count(*) filter (where c.category = $2)) desc,
				          b.capacity - coalesce(sum(k.qty), 0) asc
				 limit 1 for update of b`, p.qty, p.category).Scan(&boxID, &boxCode)
			if err != nil {
				continue // nothing has room; leave it unassigned rather than force it
			}
			if _, err := tx.Exec(r.Context(), `
				insert into public.box_contents (box_id, component_id, qty)
				values ($1, $2, $3)
				on conflict (box_id, component_id) do update set qty = excluded.qty`,
				boxID, p.id, p.qty); err != nil {
				return err
			}
			moves = append(moves, move{Component: p.name, Box: boxCode, Qty: p.qty})
		}

		if len(moves) > 0 {
			return logActivity(r, tx, itoa(len(moves))+" component(s) assigned to bins",
				"⊞", "#8da2c8", "box", "")
		}
		return nil
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]any{"moved": len(moves), "moves": moves})
}

// POST /api/boxes/:id/comments
func (h *BoxHandler) AddComment(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	addComment(w, r, "box_id", id)
}

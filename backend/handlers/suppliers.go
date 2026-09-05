package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type SupplierHandler struct{}

func (h *SupplierHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)

	r.Route("/{id}", func(sr chi.Router) {
		sr.Get("/", h.Get)
		sr.Put("/", h.Update)
		sr.Delete("/", h.Delete)
		sr.Get("/components", h.Components)
		sr.Get("/spend", h.Spend)
	})
}

// GET /api/suppliers — with holdings, so the list needs no follow-up query.
func (h *SupplierHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.SupplierColumns+`,
		       coalesce(v.skus, 0)        as skus,
		       coalesce(v.units, 0)       as units,
		       coalesce(v.stock_value, 0) as "stockValue"
		  from public.suppliers s
		  left join public.v_supplier_spend v on v.id = s.id
		 where ($1 = '' or s.name ilike '%'||$1||'%' or s.email ilike '%'||$1||'%')
		 order by s.name`, query(r, "q", ""))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/suppliers/:id
func (h *SupplierHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select `+models.SupplierColumns+`,
		       coalesce(v.skus, 0)        as skus,
		       coalesce(v.units, 0)       as units,
		       coalesce(v.stock_value, 0) as "stockValue",
		       coalesce(v.low_stock_skus, 0) as "lowStockSkus"
		  from public.suppliers s
		  left join public.v_supplier_spend v on v.id = s.id
		 where s.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/suppliers
func (h *SupplierHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.SupplierRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		insert into public.suppliers (name, contact, email, phone, website, notes)
		values ($1, $2, $3, $4, $5, $6)
		returning id, name, contact, email, phone, website, notes,
		          extract(epoch from created_at) * 1000 as created`,
		req.Name, req.Contact, req.Email, req.Phone, req.Website, req.Notes)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(), `Added supplier "`+req.Name+`"`, "＋", "#5faa87", "supplier", "")
	writeRaw(w, http.StatusCreated, body)
}

// PUT /api/suppliers/:id
func (h *SupplierHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.SupplierRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		update public.suppliers s set
		  name = $2, contact = $3, email = $4, phone = $5, website = $6, notes = $7
		 where s.id = $1
		returning s.id, s.name, s.contact, s.email, s.phone, s.website, s.notes`,
		id, req.Name, req.Contact, req.Email, req.Phone, req.Website, req.Notes)
	if err != nil {
		fail(w, r, err)
		return
	}

	// keep the denormalised name on components in step
	_, _ = db.GetDB().Exec(r.Context(),
		`update public.components set supplier_name = $2 where supplier_id = $1`, id, req.Name)
	writeRaw(w, http.StatusOK, body)
}

// DELETE /api/suppliers/:id — components keep their supplier name but lose the
// link (ON DELETE SET NULL), so no inventory disappears with a vendor.
func (h *SupplierHandler) Delete(w http.ResponseWriter, r *http.Request) {
	softDelete(w, r, "suppliers", "name")
}

// GET /api/suppliers/:id/components
func (h *SupplierHandler) Components(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.ComponentColumns+`,
		       (c.min_stock is not null and c.quantity <= c.min_stock) as "lowStock"
		  from public.components c
		  left join public.suppliers s on s.id = c.supplier_id
		 where c.supplier_id = $1
		 order by c.name`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/suppliers/:id/spend
func (h *SupplierHandler) Spend(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select id, name, skus, units, stock_value as "stockValue",
		       low_stock_skus as "lowStockSkus"
		  from public.v_supplier_spend where id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

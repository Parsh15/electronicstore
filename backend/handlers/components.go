package handlers

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
	"github.com/asksummu/electronic-store-manager/backend/services"
)

type ComponentHandler struct{}

func (h *ComponentHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Post("/import", h.Import)
	r.Get("/export", h.Export)
	r.Get("/low-stock", h.LowStock)
	r.Get("/faulty", h.Faulty)

	r.Route("/{id}", func(cr chi.Router) {
		cr.Get("/", h.Get)
		cr.Put("/", h.Update)
		cr.Delete("/", h.Delete)
		cr.Post("/restock", h.Restock)
		cr.Post("/duplicate", h.Duplicate)
		cr.Get("/units", h.Units)
		cr.Get("/where-used", h.WhereUsed)
		cr.Get("/history", h.History)
		cr.Get("/comments", h.Comments)
		cr.Post("/comments", h.AddComment)
	})
}

// GET /api/components
//
// Supports the filters the inventory screen already offers: free text, category,
// supplier, location, low-stock and faulty toggles. Everything is a bound
// parameter; the only thing the query string can change is which conditions
// apply, never the SQL itself.
func (h *ComponentHandler) List(w http.ResponseWriter, r *http.Request) {
	sortCol := map[string]string{
		"name": "c.name", "code": "c.code", "quantity": "c.quantity",
		"category": "c.category", "price": "c.price", "created": "c.created_at",
		"location": "c.location",
	}[query(r, "sort", "name")]
	if sortCol == "" {
		sortCol = "c.name"
	}
	dir := "asc"
	if strings.EqualFold(query(r, "dir", "asc"), "desc") {
		dir = "desc"
	}

	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.ComponentColumns+`,
		       (select count(*) from public.component_units u
		         where u.component_id = c.id and u.status = 'stock') as "unitsInStock",
		       (select count(*) from public.component_units u
		         where u.component_id = c.id and u.faulty) as "unitsFaulty",
		       (c.min_stock is not null and c.quantity <= c.min_stock) as "lowStock"
		  from public.components c
		  left join public.suppliers s on s.id = c.supplier_id
		 where ($1 = '' or c.name ilike '%'||$1||'%' or c.code ilike '%'||$1||'%'
		        or c.category ilike '%'||$1||'%' or c.location ilike '%'||$1||'%')
		   and ($2 = '' or c.category = $2)
		   and ($3 = '' or c.supplier_id = $3::uuid)
		   and ($4 = '' or c.location ilike '%'||$4||'%')
		   and (not $5 or (c.min_stock is not null and c.quantity <= c.min_stock))
		   and (not $6 or c.faulty)
		 order by `+sortCol+` `+dir+`
		 limit $7 offset $8`,
		query(r, "q", ""), query(r, "category", ""), uuidOrEmpty(query(r, "supplierId", "")),
		query(r, "location", ""), queryBool(r, "lowStock"), queryBool(r, "faulty"),
		queryInt(r, "limit", 2000, 1, 5000), queryInt(r, "offset", 0, 0, 1_000_000))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/components/:id — the component plus its units and comments, which is
// what the detail drawer needs in one request.
func (h *ComponentHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select `+models.ComponentColumns+`,
		       coalesce((select json_agg(t) from (
		         select `+models.UnitColumns+` from public.component_units u
		          where u.component_id = c.id order by u.unit_id) t), '[]'::json) as units,
		       coalesce((select json_agg(t) from (
		         select m.id, m.author, m.body as text, m.tag,
		                extract(epoch from m.created_at) * 1000 as ts
		           from public.comments m where m.component_id = c.id
		          order by m.created_at desc) t), '[]'::json) as comments,
		       coalesce((select json_agg(t) from (
		         select b.id, b.code, b.name, k.qty from public.box_contents k
		           join public.boxes b on b.id = k.box_id
		          where k.component_id = c.id) t), '[]'::json) as boxes
		  from public.components c
		  left join public.suppliers s on s.id = c.supplier_id
		 where c.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/components
//
// The insert and the automation run share one transaction, so a component can
// never exist without the labels, bin and reorder point the automation promised
// — nor the other way round.
func (h *ComponentHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.ComponentRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	var (
		body []byte
		plan *services.Plan
	)
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(r.Context(), `
			insert into public.components
			  (code, name, category, location, quantity, min_stock, reorder_point, price,
			   supplier_id, supplier_name, unit_tracked, faulty, image_name, expiry, datasheet, notes)
			values (coalesce(nullif($1,''), public.next_code(
			          (select comp_prefix from public.settings where id = 1), 'public.component_code_seq')),
			        $2, $3, $4, $5, coalesce($6, (select low_stock_default from public.settings where id = 1)),
			        $7, $8, $9, $10, $11, $12, $13, nullif($14,'')::date, $15, $16)
			returning id`,
			req.Code, req.Name, req.Category, req.Location, req.Quantity, req.MinStock,
			req.ReorderPoint, req.Price, uuidOrNil(req.SupplierID), req.Supplier,
			req.UnitTracked, req.Faulty, req.ImageName, req.Expiry, req.Datasheet, req.Notes,
		).Scan(&id); err != nil {
			return err
		}

		if !req.SkipAutomation && req.Quantity > 0 {
			auto, err := services.LoadAutomation(r.Context(), tx)
			if err != nil {
				return err
			}
			plan, err = services.AutoRun(r.Context(), tx, id, req.Quantity, auto, actorID(r))
			if err != nil {
				return err
			}
		}

		if _, err := tx.Exec(r.Context(), `
			insert into public.activity (body, glyph, color, actor, actor_id, entity_type, entity_id)
			values ($1, '＋', '#5faa87', $2, $3, 'component', $4)`,
			`Added "`+req.Name+`"`, actor(r), uuidOrNil(ptr(actorID(r))), id); err != nil {
			return err
		}

		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.ComponentColumns+`
			   from public.components c
			   left join public.suppliers s on s.id = c.supplier_id
			  where c.id = $1`, id)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	created(w, map[string]any{"component": rawJSON(body), "automation": plan})
}

// PUT /api/components/:id
//
// A quantity increase through the editor runs the automation too, matching what
// the UI has always done after a save.
func (h *ComponentHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.ComponentRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	var (
		body []byte
		plan *services.Plan
	)
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var before int
		if err := tx.QueryRow(r.Context(),
			`select quantity from public.components where id = $1 for update`, id).Scan(&before); err != nil {
			return err
		}

		if _, err := tx.Exec(r.Context(), `
			update public.components set
			  name = $2, category = $3, location = $4, quantity = $5,
			  min_stock = $6, reorder_point = coalesce($7, reorder_point), price = $8,
			  supplier_id = $9, supplier_name = $10, unit_tracked = $11, faulty = $12,
			  image_name = $13, expiry = nullif($14,'')::date, datasheet = $15, notes = $16
			 where id = $1`,
			id, req.Name, req.Category, req.Location, req.Quantity, req.MinStock,
			req.ReorderPoint, req.Price, uuidOrNil(req.SupplierID), req.Supplier,
			req.UnitTracked, req.Faulty, req.ImageName, req.Expiry, req.Datasheet, req.Notes,
		); err != nil {
			return err
		}

		if added := req.Quantity - before; added > 0 && !req.SkipAutomation {
			auto, err := services.LoadAutomation(r.Context(), tx)
			if err != nil {
				return err
			}
			plan, err = services.AutoRun(r.Context(), tx, id, added, auto, actorID(r))
			if err != nil {
				return err
			}
		}

		if _, err := tx.Exec(r.Context(), `
			insert into public.activity (body, glyph, color, actor, actor_id, entity_type, entity_id)
			values ($1, '✎', '#8da2c8', $2, $3, 'component', $4)`,
			`Edited "`+req.Name+`"`, actor(r), uuidOrNil(ptr(actorID(r))), id); err != nil {
			return err
		}

		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.ComponentColumns+`
			   from public.components c
			   left join public.suppliers s on s.id = c.supplier_id
			  where c.id = $1`, id)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]any{"component": rawJSON(body), "automation": plan})
}

// DELETE /api/components/:id — soft delete, so it lands in the trash and can be
// restored. The units and comments travel with it.
func (h *ComponentHandler) Delete(w http.ResponseWriter, r *http.Request) {
	softDelete(w, r, "components", "name")
}

// POST /api/components/:id/restock
//
// The stored function moves the quantity, mints unit IDs and writes the note;
// the automation runs in the same transaction for the added amount only.
func (h *ComponentHandler) Restock(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.RestockRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	var (
		body []byte
		plan *services.Plan
	)
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(),
			`select public.restock_component($1, $2, $3, $4)`,
			id, req.Add, nullString(req.Note), actor(r)); err != nil {
			return err
		}

		if req.Add > 0 {
			auto, err := services.LoadAutomation(r.Context(), tx)
			if err != nil {
				return err
			}
			// restock_component already created the unit rows, so units are
			// off here to avoid a second set.
			off := false
			auto.Units = &off
			plan, err = services.AutoRun(r.Context(), tx, id, req.Add, auto, actorID(r))
			if err != nil {
				return err
			}
			plan.UnitsCreated = req.Add
		}

		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.ComponentColumns+`
			   from public.components c
			   left join public.suppliers s on s.id = c.supplier_id
			  where c.id = $1`, id)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]any{"component": rawJSON(body), "automation": plan})
}

// POST /api/components/:id/duplicate — copies the record with a fresh code and
// no stock, which is how the UI seeds a variant of an existing part.
func (h *ComponentHandler) Duplicate(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var body []byte
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var newID string
		if err := tx.QueryRow(r.Context(), `
			insert into public.components
			  (code, name, category, location, quantity, min_stock, reorder_point, price,
			   supplier_id, supplier_name, unit_tracked, image_name, datasheet, notes)
			select public.next_code((select comp_prefix from public.settings where id = 1),
			                        'public.component_code_seq'),
			       name || ' (copy)', category, location, 0, min_stock, reorder_point, price,
			       supplier_id, supplier_name, unit_tracked, image_name, datasheet, notes
			  from public.components where id = $1
			returning id`, id).Scan(&newID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `
			insert into public.activity (body, glyph, color, actor, actor_id, entity_type, entity_id)
			select 'Duplicated "' || name || '"', '⧉', '#8da2c8', $2, $3, 'component', $1::text
			  from public.components where id = $1`,
			newID, actor(r), uuidOrNil(ptr(actorID(r)))); err != nil {
			return err
		}
		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.ComponentColumns+`
			   from public.components c
			   left join public.suppliers s on s.id = c.supplier_id
			  where c.id = $1`, newID)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusCreated, body)
}

// GET /api/components/:id/units
func (h *ComponentHandler) Units(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(),
		`select `+models.UnitColumns+`, p.name as "projectName"
		   from public.component_units u
		   left join public.projects p on p.id = u.project_id
		  where u.component_id = $1
		  order by u.unit_id`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/components/:id/where-used
func (h *ComponentHandler) WhereUsed(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select project_id as "projectId", project_code as "projectCode",
		       project_name as "projectName", project_status as "projectStatus",
		       qty, part_status as "partStatus"
		  from public.v_where_used where component_id = $1
		 order by project_name`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/components/:id/history — this component's slice of the audit trail.
func (h *ComponentHandler) History(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(),
		`select `+models.ActivityColumns+`
		   from public.activity a
		  where a.entity_type = 'component' and a.entity_id = $1
		  order by a.created_at desc limit $2`, id, queryInt(r, "limit", 100, 1, 500))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/components/:id/comments
func (h *ComponentHandler) Comments(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select m.id, m.author, m.body as text, m.tag,
		       extract(epoch from m.created_at) * 1000 as ts
		  from public.comments m where m.component_id = $1
		 order by m.created_at desc`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/components/:id/comments
func (h *ComponentHandler) AddComment(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	addComment(w, r, "component_id", id)
}

// GET /api/components/low-stock
func (h *ComponentHandler) LowStock(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select id, code, name, category, location, quantity,
		       min_stock as "minStock", deficit, suggested_order as "suggestedOrder",
		       suggested_order_cost as "suggestedOrderCost",
		       supplier, supplier_email as "supplierEmail", price
		  from public.v_low_stock limit $1`, queryInt(r, "limit", 500, 1, 2000))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/components/faulty
func (h *ComponentHandler) Faulty(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.ComponentColumns+`,
		       (select count(*) from public.component_units u
		         where u.component_id = c.id and u.faulty) as "unitsFaulty"
		  from public.components c
		  left join public.suppliers s on s.id = c.supplier_id
		 where c.faulty
		    or exists (select 1 from public.component_units u
		                where u.component_id = c.id and u.faulty)
		 order by c.name`)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/components/export — CSV of the whole inventory, streamed.
func (h *ComponentHandler) Export(w http.ResponseWriter, r *http.Request) {
	cols, rows, err := db.Rows(r.Context(), db.GetDB(), `
		select code, name, category, location, quantity, min_stock, reorder_point,
		       price, stock_value, supplier, unit_tracked, faulty, low_stock
		  from public.v_component_stock
		 order by category, name`)
	if err != nil {
		fail(w, r, err)
		return
	}
	download(w, "inventory.csv", "text/csv; charset=utf-8")
	writeCSVRows(w, cols, rows)
}

// POST /api/components/import
//
// Smart Import: every row is validated before anything is written (see
// ImportRequest.Validate), then the whole file lands in one transaction. A
// failure on row 900 leaves rows 1-899 unwritten, which is what makes it safe
// to retry a corrected file.
func (h *ComponentHandler) Import(w http.ResponseWriter, r *http.Request) {
	var req models.ImportRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	type outcome struct {
		Inserted   int              `json:"inserted"`
		Updated    int              `json:"updated"`
		Automation *services.Plan   `json:"automation"`
		Plans      []*services.Plan `json:"-"`
	}
	res := outcome{Automation: &services.Plan{}}

	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		auto, err := services.LoadAutomation(r.Context(), tx)
		if err != nil {
			return err
		}

		for i := range req.Rows {
			row := req.Rows[i]

			var id string
			if req.Merge {
				err := tx.QueryRow(r.Context(),
					`select id from public.components where lower(name) = lower($1) limit 1`,
					row.Name).Scan(&id)
				if err != nil && err.Error() != pgx.ErrNoRows.Error() {
					return err
				}
			}

			if id != "" {
				if _, err := tx.Exec(r.Context(), `
					update public.components
					   set quantity = quantity + $2,
					       category = coalesce(nullif($3,''), category),
					       location = coalesce(nullif($4,''), location),
					       min_stock = coalesce($5, min_stock),
					       price = coalesce($6, price),
					       supplier_name = coalesce(nullif($7,''), supplier_name)
					 where id = $1`,
					id, row.Quantity, row.Category, row.Location,
					row.MinStock, row.Price, row.Supplier); err != nil {
					return err
				}
				res.Updated++
			} else {
				if err := tx.QueryRow(r.Context(), `
					insert into public.components
					  (code, name, category, location, quantity, min_stock, price,
					   supplier_name, unit_tracked, notes)
					values (public.next_code((select comp_prefix from public.settings where id = 1),
					                         'public.component_code_seq'),
					        $1, $2, $3, $4,
					        coalesce($5, (select low_stock_default from public.settings where id = 1)),
					        $6, $7, $8, $9)
					returning id`,
					row.Name, row.Category, row.Location, row.Quantity, row.MinStock,
					row.Price, row.Supplier, row.UnitTracked, row.Notes).Scan(&id); err != nil {
					return err
				}
				res.Inserted++
			}

			if !req.SkipAutomation && row.Quantity > 0 {
				plan, err := services.AutoRun(r.Context(), tx, id, row.Quantity, auto, actorID(r))
				if err != nil {
					return err
				}
				res.Automation.LabelsQueued += plan.LabelsQueued
				res.Automation.UnitsCreated += plan.UnitsCreated
				res.Automation.AutomationsRan += plan.AutomationsRan
				if plan.BinAssigned != "" {
					res.Automation.BinAssigned = plan.BinAssigned
				}
			}
		}

		// link suppliers named in the file to real supplier records
		if _, err := tx.Exec(r.Context(), `
			update public.components c set supplier_id = s.id
			  from public.suppliers s
			 where lower(s.name) = lower(c.supplier_name) and c.supplier_id is null`); err != nil {
			return err
		}

		_, err = tx.Exec(r.Context(), `
			insert into public.activity (body, glyph, color, actor, actor_id, entity_type)
			values ($1, '⇪', '#5faa87', $2, $3, 'import')`,
			importSummary(res.Inserted, res.Updated), actor(r), uuidOrNil(ptr(actorID(r))))
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, res)
}

func importSummary(inserted, updated int) string {
	var b strings.Builder
	b.WriteString("Imported ")
	b.WriteString(itoa(inserted))
	b.WriteString(" component(s)")
	if updated > 0 {
		b.WriteString(", updated ")
		b.WriteString(itoa(updated))
	}
	return b.String()
}

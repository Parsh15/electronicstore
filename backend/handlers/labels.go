package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
	"github.com/asksummu/electronic-store-manager/backend/services"
)

type LabelHandler struct{}

func (h *LabelHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/generate", h.Generate)
	r.Post("/print-queue", h.PrintQueue)
	r.Get("/by-component/{id}", h.ByComponent)
	r.Get("/by-unit/{id}", h.ByUnit)
	r.Get("/by-box/{id}", h.ByBox)

	r.Route("/{id}", func(lr chi.Router) {
		lr.Get("/", h.Get)
		lr.Delete("/", h.Delete)
	})
}

const labelColumns = `l.id, l.label_id as "labelId", l.type,
	l.component_id as "componentId", l.unit_id as "unitId", l.box_id as "boxId",
	l.data, l.printed, extract(epoch from l.created_at) * 1000 as created`

// GET /api/labels
func (h *LabelHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+labelColumns+`
		  from public.labels l
		 where ($1 = '' or l.type = $1)
		   and (not $2 or not l.printed)
		 order by l.created_at desc
		 limit $3`,
		query(r, "type", ""), queryBool(r, "unprinted"), queryInt(r, "limit", 500, 1, 5000))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/labels/:id
func (h *LabelHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(),
		`select `+labelColumns+` from public.labels l where l.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/labels/generate
//
// Builds the label records and their QR payloads. The QR content is stored as a
// string; a base64 PNG is added only when the caller asks for one with
// ?png=true, because the frontend already renders codes from the string.
func (h *LabelHandler) Generate(w http.ResponseWriter, r *http.Request) {
	var req models.LabelRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}
	withPNG := queryBool(r, "png")

	var body []byte
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		var ids []string

		switch req.Type {
		case "component", "batch":
			var code, name, category, location string
			if err := tx.QueryRow(r.Context(), `
				select code, name, category, coalesce(location, '')
				  from public.components where id = $1`, req.ComponentID).
				Scan(&code, &name, &category, &location); err != nil {
				return err
			}
			var next int
			if err := tx.QueryRow(r.Context(),
				`select count(*) from public.labels where component_id = $1`,
				req.ComponentID).Scan(&next); err != nil {
				return err
			}
			for i := 0; i < req.Qty; i++ {
				labelID := fmt.Sprintf("%s-L%03d", code, next+i+1)
				qr := services.QRUnit("", req.ComponentID)
				payload := map[string]any{
					"labelId": labelID, "type": req.Type, "code": code, "name": name,
					"category": category, "location": location, "size": req.Size,
					"qrContent": qrComponentJSON(req.ComponentID, location),
				}
				_ = qr
				if withPNG {
					if png, err := services.QRPNG(payload["qrContent"].(string), 256); err == nil {
						payload["qrPng"] = png
					}
				}
				id, err := insertLabel(r, tx, labelID, req.Type, payload,
					req.ComponentID, "", "")
				if err != nil {
					return err
				}
				if id != "" {
					ids = append(ids, id)
				}
			}

		case "unit":
			var unitCode, compID, compName, compCode string
			if err := tx.QueryRow(r.Context(), `
				select u.unit_id, u.component_id, c.name, c.code
				  from public.component_units u
				  join public.components c on c.id = u.component_id
				 where u.id = $1`, req.UnitID).
				Scan(&unitCode, &compID, &compName, &compCode); err != nil {
				return err
			}
			payload := map[string]any{
				"labelId": unitCode, "type": "unit", "unitId": unitCode,
				"componentCode": compCode, "name": compName, "size": req.Size,
				"qrContent": services.QRUnit(unitCode, compID),
			}
			if withPNG {
				if png, err := services.QRPNG(payload["qrContent"].(string), 256); err == nil {
					payload["qrPng"] = png
				}
			}
			id, err := insertLabel(r, tx, unitCode, "unit", payload, compID, req.UnitID, "")
			if err != nil {
				return err
			}
			if id != "" {
				ids = append(ids, id)
			}

		case "box":
			var boxCode, boxName, location string
			if err := tx.QueryRow(r.Context(),
				`select code, name, coalesce(location, '') from public.boxes where id = $1`,
				req.BoxID).Scan(&boxCode, &boxName, &location); err != nil {
				return err
			}
			payload := map[string]any{
				"labelId": boxCode, "type": "box", "code": boxCode, "name": boxName,
				"location": location, "size": req.Size,
				"qrContent": services.QRBox(req.BoxID),
			}
			if withPNG {
				if png, err := services.QRPNG(payload["qrContent"].(string), 256); err == nil {
					payload["qrPng"] = png
				}
			}
			id, err := insertLabel(r, tx, boxCode, "box", payload, "", "", req.BoxID)
			if err != nil {
				return err
			}
			if id != "" {
				ids = append(ids, id)
			}
		}

		if err := logActivity(r, tx, itoa(len(ids))+" label(s) generated",
			"▤", "#8da2c8", "label", ""); err != nil {
			return err
		}

		var err error
		body, err = db.QueryJSON(r.Context(), tx,
			`select `+labelColumns+` from public.labels l where l.id = any($1::uuid[])
			  order by l.label_id`, ids)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusCreated, body)
}

func insertLabel(r *http.Request, tx pgx.Tx, labelID, kind string, payload map[string]any,
	componentID, unitID, boxID string) (string, error) {

	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(r.Context(), `
		insert into public.labels (label_id, type, component_id, unit_id, box_id, data, created_by)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (label_id) do update set data = excluded.data
		returning id`,
		labelID, kind, uuidOrNil(&componentID), uuidOrNil(&unitID), uuidOrNil(&boxID),
		data, uuidOrNil(ptr(actorID(r)))).Scan(&id)
	return id, err
}

func qrComponentJSON(id, bin string) string {
	b, _ := json.Marshal(map[string]string{"type": "component", "id": id, "bin": bin})
	return string(b)
}

// DELETE /api/labels/:id
func (h *LabelHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	tag, err := db.GetDB().Exec(r.Context(), `delete from public.labels where id = $1`, id)
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

// POST /api/labels/print-queue — marks a selection printed (or un-printed).
func (h *LabelHandler) PrintQueue(w http.ResponseWriter, r *http.Request) {
	var req models.PrintQueueRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	tag, err := db.GetDB().Exec(r.Context(), `
		update public.labels set printed = $2
		 where id::text = any($1) or label_id = any($1)`, req.LabelIDs, req.Printed)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(), itoa(int(tag.RowsAffected()))+" label(s) queued for printing",
		"⎙", "#8da2c8", "label", "")
	ok(w, map[string]int64{"updated": tag.RowsAffected()})
}

// GET /api/labels/by-component/:id — including every unit label for that part.
func (h *LabelHandler) ByComponent(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+labelColumns+`
		  from public.labels l
		 where l.component_id = $1
		    or l.unit_id in (select u.id from public.component_units u where u.component_id = $1)
		 order by l.type, l.label_id`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/labels/by-unit/:id
func (h *LabelHandler) ByUnit(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(),
		`select `+labelColumns+` from public.labels l where l.unit_id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/labels/by-box/:id
func (h *LabelHandler) ByBox(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSON(r.Context(), db.GetDB(),
		`select `+labelColumns+` from public.labels l where l.box_id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

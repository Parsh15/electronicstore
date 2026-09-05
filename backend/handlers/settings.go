package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

type SettingsHandler struct{}

// Reads are open to any signed-in member — the currency symbol and date format
// are needed to render almost every screen. Writes are admin-only, applied by
// the router.
func (h *SettingsHandler) Routes(r chi.Router) {
	r.Get("/", h.Get)
	r.Get("/automation", h.GetAutomation)

	r.Group(func(ar chi.Router) {
		ar.Use(middleware.Admin)
		ar.Put("/", h.Update)
		ar.Put("/automation", h.UpdateAutomation)
	})
}

// GET /api/settings
func (h *SettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(),
		`select `+models.SettingsColumns+` from public.settings s where s.id = 1`)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// PUT /api/settings — a patch: fields left out keep their current value.
func (h *SettingsHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req models.SettingsRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		update public.settings s set
		  currency_symbol   = coalesce($1, s.currency_symbol),
		  low_stock_default = coalesce($2, s.low_stock_default),
		  comp_prefix       = coalesce($3, s.comp_prefix),
		  box_prefix        = coalesce($4, s.box_prefix),
		  date_fmt          = coalesce($5, s.date_fmt)
		 where s.id = 1
		returning `+models.SettingsColumns,
		req.CurrencySymbol, req.LowStockDefault, req.CompPrefix, req.BoxPrefix, req.DateFmt)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(), "Store settings updated", "⚙", "#8da2c8", "settings", "")
	writeRaw(w, http.StatusOK, body)
}

// GET /api/settings/automation
func (h *SettingsHandler) GetAutomation(w http.ResponseWriter, r *http.Request) {
	body, err := db.ScalarJSON(r.Context(), db.GetDB(),
		`select automation::text from public.settings where id = 1`)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// PUT /api/settings/automation
//
// Merged into the existing object rather than replacing it, so turning one
// rule off does not silently reset the others.
func (h *SettingsHandler) UpdateAutomation(w http.ResponseWriter, r *http.Request) {
	var req models.AutomationRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	patch := map[string]any{}
	if req.Labels != nil {
		patch["labels"] = *req.Labels
	}
	if req.Bins != nil {
		patch["bins"] = *req.Bins
	}
	if req.Reorder != nil {
		patch["reorder"] = *req.Reorder
	}
	if req.Units != nil {
		patch["units"] = *req.Units
	}
	if req.BoxCapacity != nil {
		patch["boxCapacity"] = *req.BoxCapacity
	}
	if len(patch) == 0 {
		bad(w, models.Invalid("", "Nothing to change."))
		return
	}

	body, err := db.ScalarJSON(r.Context(), db.GetDB(), `
		update public.settings
		   set automation = automation || $1::jsonb
		 where id = 1
		returning automation::text`, patch)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(), "Automation rules updated", "⚙", "#8da2c8", "settings", "")
	writeRaw(w, http.StatusOK, body)
}

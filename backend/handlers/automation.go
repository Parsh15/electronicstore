package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
	"github.com/asksummu/electronic-store-manager/backend/models"
	"github.com/asksummu/electronic-store-manager/backend/services"
)

type AutomationHandler struct{}

func (h *AutomationHandler) Routes(r chi.Router) {
	r.Get("/log", h.Log)
	r.Get("/plan/{componentId}", h.Plan)
	r.With(middleware.Admin).Post("/run", h.Run)
}

// GET /api/automation/log — what the engine did, newest first.
func (h *AutomationHandler) Log(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select g.id, g.body as text, g.kind, g.entity_id as "entityId", g.detail,
		       extract(epoch from g.created_at) * 1000 as ts
		  from public.automation_log g
		 where ($1 = '' or g.kind = $1)
		 order by g.created_at desc
		 limit $2`, query(r, "kind", ""), queryInt(r, "limit", 100, 1, 500))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/automation/plan/:componentId
//
// A dry run: what the engine would do for this component's current quantity.
// Writes nothing, so the UI can show the plan before anyone commits.
func (h *AutomationHandler) Plan(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "componentId")
	if !valid {
		return
	}
	plan, err := services.PlanFor(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, plan)
}

// POST /api/automation/run
//
// Runs the engine against a component on demand — the "re-run automation"
// action in the admin section. Admin-only, because it can mint labels and open
// bins across the store.
func (h *AutomationHandler) Run(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ComponentID string `json:"componentId"`
		All         bool   `json:"all"`
	}
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if !req.All {
		if p := models.ValidUUID("componentId", req.ComponentID); p != nil {
			bad(w, p)
			return
		}
	}

	total := &services.Plan{}
	touched := 0

	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		auto, err := services.LoadAutomation(r.Context(), tx)
		if err != nil {
			return err
		}

		type target struct {
			id  string
			qty int
		}
		var targets []target

		if req.All {
			rows, err := tx.Query(r.Context(),
				`select id, quantity from public.components where quantity > 0 order by name`)
			if err != nil {
				return err
			}
			for rows.Next() {
				var t target
				if err := rows.Scan(&t.id, &t.qty); err != nil {
					rows.Close()
					return err
				}
				targets = append(targets, t)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		} else {
			var t target
			t.id = req.ComponentID
			if err := tx.QueryRow(r.Context(),
				`select quantity from public.components where id = $1`, t.id).Scan(&t.qty); err != nil {
				return err
			}
			targets = append(targets, t)
		}

		for _, t := range targets {
			plan, err := services.AutoRun(r.Context(), tx, t.id, t.qty, auto, actorID(r))
			if err != nil {
				return err
			}
			total.LabelsQueued += plan.LabelsQueued
			total.UnitsCreated += plan.UnitsCreated
			total.AutomationsRan += plan.AutomationsRan
			if plan.BinAssigned != "" {
				total.BinAssigned = plan.BinAssigned
			}
			if plan.ReorderPoint > 0 {
				total.ReorderPoint = plan.ReorderPoint
			}
			touched++
		}

		return logActivity(r, tx,
			"Automation re-run across "+itoa(touched)+" component(s)",
			"⚡", "#8da2c8", "automation", "")
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]any{"components": touched, "automation": total})
}

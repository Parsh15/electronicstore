package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
	"github.com/asksummu/electronic-store-manager/backend/services"
)

type ReportHandler struct{}

func (h *ReportHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/generate", h.Generate)

	// The named shortcuts the reports screen links to directly. They come
	// before /{id} so "inventory" is never read as a record id.
	r.Get("/inventory", h.quick("inventory"))
	r.Get("/low-stock", h.quick("low-stock"))
	r.Get("/valuation", h.quick("valuation"))
	r.Get("/bom", h.quick("bom"))
	r.Get("/supplier", h.quick("supplier"))
	r.Get("/audit", h.quick("audit"))

	r.Route("/{id}", func(rr chi.Router) {
		rr.Get("/", h.Get)
		rr.Get("/download", h.Download)
		rr.Delete("/", h.Delete)
	})
}

// GET /api/reports — saved reports, newest first.
func (h *ReportHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select rp.id, rp.type, rp.name,
		       coalesce(p.name, '') as "generatedBy",
		       extract(epoch from rp.generated_at) * 1000 as "generatedAt"
		  from public.reports rp
		  left join public.profiles p on p.id = rp.user_id
		 where ($1 = '' or rp.type = $1)
		 order by rp.generated_at desc
		 limit $2`, query(r, "type", ""), queryInt(r, "limit", 100, 1, 500))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/reports/generate
//
// One request produces the report; format decides whether it comes back as
// JSON for the screen, CSV for a spreadsheet, or PDF for printing. Same data
// in all three cases.
func (h *ReportHandler) Generate(w http.ResponseWriter, r *http.Request) {
	var req models.ReportRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}
	h.render(w, r, &req)
}

// quick builds a GET shortcut for one report type, reading its options from the
// query string instead of a body.
func (h *ReportHandler) quick(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req := models.ReportRequest{
			Type:      kind,
			ProjectID: uuidOrEmpty(query(r, "projectId", "")),
			From:      query(r, "from", ""),
			To:        query(r, "to", ""),
			UserID:    uuidOrEmpty(query(r, "userId", "")),
			Format:    query(r, "format", "json"),
			Save:      queryBool(r, "save"),
		}
		if p := req.Validate(); p != nil {
			bad(w, p)
			return
		}
		h.render(w, r, &req)
	}
}

func (h *ReportHandler) render(w http.ResponseWriter, r *http.Request, req *models.ReportRequest) {
	report, err := services.Generate(r.Context(), req)
	if err != nil {
		fail(w, r, err)
		return
	}

	if req.Save {
		if id, err := services.Save(r.Context(), actorID(r), report); err == nil {
			w.Header().Set("X-Report-Id", id)
		}
	}

	switch req.Format {
	case "csv":
		download(w, slug(report.Name)+".csv", "text/csv; charset=utf-8")
		if err := services.WriteCSV(w, report); err != nil {
			return // headers are already out; nothing useful left to say
		}
	case "pdf":
		download(w, slug(report.Name)+".pdf", "application/pdf")
		if err := services.WritePDF(w, report); err != nil {
			return
		}
	default:
		ok(w, report)
	}
}

// GET /api/reports/:id
func (h *ReportHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		select rp.id, rp.type, rp.name, rp.data,
		       extract(epoch from rp.generated_at) * 1000 as "generatedAt"
		  from public.reports rp where rp.id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/reports/:id/download?format=csv|pdf — re-renders a saved report from
// the stored snapshot, so a download always matches what was generated.
func (h *ReportHandler) Download(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	report, err := services.Load(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}

	switch query(r, "format", "csv") {
	case "pdf":
		download(w, slug(report.Name)+".pdf", "application/pdf")
		_ = services.WritePDF(w, report)
	case "json":
		download(w, slug(report.Name)+".json", "application/json")
		ok(w, report)
	default:
		download(w, slug(report.Name)+".csv", "text/csv; charset=utf-8")
		_ = services.WriteCSV(w, report)
	}
}

// DELETE /api/reports/:id
func (h *ReportHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	tag, err := db.GetDB().Exec(r.Context(), `delete from public.reports where id = $1`, id)
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

// slug turns a report name into a safe filename.
func slug(s string) string {
	out := make([]rune, 0, len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
			prevDash = false
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
			prevDash = false
		default:
			if !prevDash && len(out) > 0 {
				out = append(out, '-')
				prevDash = true
			}
		}
	}
	res := string(out)
	for len(res) > 0 && res[len(res)-1] == '-' {
		res = res[:len(res)-1]
	}
	if res == "" {
		return "report"
	}
	return res
}

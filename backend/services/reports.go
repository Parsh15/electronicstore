package services

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jung-kurt/gofpdf"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

// All six reports, generated server-side.
//
// Each one reads a view that already does the arithmetic (see database/views.sql)
// and adds only the grouping and totals the report itself needs. The same
// structured result feeds the JSON, CSV and PDF renderings, so the three can
// never disagree about a number.

type Section struct {
	Title   string           `json:"title"`
	Columns []string         `json:"columns"`
	Rows    [][]any          `json:"rows"`
	Totals  map[string]any   `json:"totals,omitempty"`
	Meta    map[string]any   `json:"meta,omitempty"`
}

type Report struct {
	Type        string    `json:"type"`
	Name        string    `json:"name"`
	GeneratedAt time.Time `json:"generatedAt"`
	Currency    string    `json:"currency"`
	Summary     map[string]any `json:"summary"`
	Sections    []Section `json:"sections"`
}

// Generate dispatches on type. Anything not in models.ReportTypes was already
// rejected by validation.
func Generate(ctx context.Context, req *models.ReportRequest) (*Report, error) {
	currency, err := currencySymbol(ctx)
	if err != nil {
		return nil, err
	}
	r := &Report{Type: req.Type, GeneratedAt: time.Now(), Currency: currency,
		Summary: map[string]any{}}

	switch req.Type {
	case "inventory":
		r.Name = orDefault(req.Name, "Inventory Report")
		err = inventory(ctx, r)
	case "low-stock":
		r.Name = orDefault(req.Name, "Low Stock Report")
		err = lowStock(ctx, r)
	case "valuation":
		r.Name = orDefault(req.Name, "Valuation Report")
		err = valuation(ctx, r)
	case "bom":
		r.Name = orDefault(req.Name, "Bill of Materials")
		err = bom(ctx, r, req.ProjectID)
	case "supplier":
		r.Name = orDefault(req.Name, "Supplier Report")
		err = supplier(ctx, r)
	case "audit":
		r.Name = orDefault(req.Name, "Audit Report")
		err = audit(ctx, r, req.From, req.To, req.UserID)
	default:
		return nil, fmt.Errorf("unknown report type %q", req.Type)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ------------------------------------------------------------------ inventory

func inventory(ctx context.Context, r *Report) error {
	cols, rows, err := db.Rows(ctx, db.GetDB(), `
		select category, code, name, location, quantity, min_stock, price, stock_value,
		       supplier, units_in_stock, units_faulty, low_stock
		  from public.v_component_stock
		 order by category, name`)
	if err != nil {
		return err
	}

	// one section per category, with a subtotal each
	byCat := map[string][][]any{}
	var order []string
	catIdx := indexOf(cols, "category")
	for _, row := range rows {
		cat, _ := row[catIdx].(string)
		if _, seen := byCat[cat]; !seen {
			order = append(order, cat)
		}
		byCat[cat] = append(byCat[cat], row[1:]) // drop the category column
	}

	valueIdx := indexOf(cols, "stock_value") - 1
	qtyIdx := indexOf(cols, "quantity") - 1
	var grandValue float64
	var grandUnits int64

	for _, cat := range order {
		sec := Section{Title: cat, Columns: cols[1:], Rows: byCat[cat]}
		var value float64
		var units int64
		for _, row := range byCat[cat] {
			value += toFloat(row[valueIdx])
			units += toInt(row[qtyIdx])
		}
		sec.Totals = map[string]any{"skus": len(byCat[cat]), "units": units, "value": round2(value)}
		grandValue += value
		grandUnits += units
		r.Sections = append(r.Sections, sec)
	}

	r.Summary = map[string]any{
		"categories": len(order), "skus": len(rows),
		"units": grandUnits, "totalValue": round2(grandValue),
	}
	return nil
}

// ------------------------------------------------------------------ low stock

func lowStock(ctx context.Context, r *Report) error {
	cols, rows, err := db.Rows(ctx, db.GetDB(), `
		select code, name, category, location, quantity, min_stock, deficit,
		       suggested_order, suggested_order_cost, supplier, supplier_email, supplier_phone
		  from public.v_low_stock`)
	if err != nil {
		return err
	}

	costIdx := indexOf(cols, "suggested_order_cost")
	var total float64
	for _, row := range rows {
		total += toFloat(row[costIdx])
	}

	r.Sections = []Section{{
		Title: "Below minimum stock", Columns: cols, Rows: rows,
		Totals: map[string]any{"items": len(rows), "orderCost": round2(total)},
	}}

	// grouped by supplier, so the report can be acted on as purchase orders
	scols, srows, err := db.Rows(ctx, db.GetDB(), `
		select coalesce(supplier, '—') as supplier, supplier_email,
		       count(*) as items, sum(suggested_order) as order_units,
		       round(sum(suggested_order_cost), 2) as order_cost
		  from public.v_low_stock
		 group by supplier, supplier_email
		 order by order_cost desc nulls last`)
	if err != nil {
		return err
	}
	r.Sections = append(r.Sections, Section{Title: "Suggested orders by supplier", Columns: scols, Rows: srows})

	r.Summary = map[string]any{"itemsBelowMinimum": len(rows), "estimatedOrderCost": round2(total)}
	return nil
}

// ------------------------------------------------------------------ valuation

func valuation(ctx context.Context, r *Report) error {
	ccols, crows, err := db.Rows(ctx, db.GetDB(), `
		select category, skus, units, value, avg_price from public.v_valuation`)
	if err != nil {
		return err
	}
	r.Sections = append(r.Sections, Section{Title: "By category", Columns: ccols, Rows: crows})

	scols, srows, err := db.Rows(ctx, db.GetDB(), `
		select name as supplier, skus, units, stock_value
		  from public.v_supplier_spend
		 where skus > 0
		 order by stock_value desc nulls last`)
	if err != nil {
		return err
	}
	r.Sections = append(r.Sections, Section{Title: "By supplier", Columns: scols, Rows: srows})

	tcols, trows, err := db.Rows(ctx, db.GetDB(), `
		select code, name, category, quantity, price, stock_value
		  from public.v_component_stock
		 where stock_value > 0
		 order by stock_value desc
		 limit 10`)
	if err != nil {
		return err
	}
	r.Sections = append(r.Sections, Section{Title: "Top 10 components by value", Columns: tcols, Rows: trows})

	var total float64
	var units int64
	if err := db.GetDB().QueryRow(ctx,
		`select coalesce(sum(value), 0), coalesce(sum(units), 0) from public.v_valuation`).
		Scan(&total, &units); err != nil {
		return err
	}
	r.Summary = map[string]any{"totalValue": round2(total), "totalUnits": units}
	return nil
}

// ------------------------------------------------------------------------ bom

func bom(ctx context.Context, r *Report, projectID string) error {
	args := []any{}
	filter := ""
	if projectID != "" {
		filter = " where project_id = $1"
		args = append(args, projectID)
	}

	pcols, prows, err := db.Rows(ctx, db.GetDB(), `
		select project_code, project_name, status, line_items, total_parts,
		       bom_cost, short_lines, buildable
		  from public.v_project_cost`+filter+`
		 order by project_name`, args...)
	if err != nil {
		return err
	}
	r.Sections = append(r.Sections, Section{Title: "Projects", Columns: pcols, Rows: prows})

	// one section per project, so the report prints as a set of pick lists
	names := []string{}
	ids := map[string]string{}
	nameIdx := indexOf(pcols, "project_name")
	codeIdx := indexOf(pcols, "project_code")
	for _, row := range prows {
		n, _ := row[nameIdx].(string)
		c, _ := row[codeIdx].(string)
		names = append(names, n)
		ids[n] = c
	}

	for _, name := range names {
		lcols, lrows, err := db.Rows(ctx, db.GetDB(), `
			select component_code, component_name, category, location, qty, on_hand,
			       short_by, line_cost, part_status, coverable
			  from public.v_project_bom
			 where project_name = $1
			 order by component_name`, name)
		if err != nil {
			return err
		}
		var cost float64
		short := 0
		ci := indexOf(lcols, "line_cost")
		si := indexOf(lcols, "coverable")
		for _, row := range lrows {
			cost += toFloat(row[ci])
			if b, ok := row[si].(bool); ok && !b {
				short++
			}
		}
		r.Sections = append(r.Sections, Section{
			Title:   ids[name] + " · " + name,
			Columns: lcols, Rows: lrows,
			Totals:  map[string]any{"lines": len(lrows), "cost": round2(cost), "shortLines": short},
			Meta:    map[string]any{"buildable": short == 0},
		})
	}

	var totalCost float64
	if err := db.GetDB().QueryRow(ctx,
		`select coalesce(sum(bom_cost), 0) from public.v_project_cost`+filter, args...).
		Scan(&totalCost); err != nil {
		return err
	}
	r.Summary = map[string]any{"projects": len(names), "totalBomCost": round2(totalCost)}
	return nil
}

// ------------------------------------------------------------------- supplier

func supplier(ctx context.Context, r *Report) error {
	cols, rows, err := db.Rows(ctx, db.GetDB(), `
		select name, email, phone, website, skus, units, stock_value, low_stock_skus
		  from public.v_supplier_spend`)
	if err != nil {
		return err
	}
	r.Sections = append(r.Sections, Section{Title: "Suppliers", Columns: cols, Rows: rows})

	nameIdx := indexOf(cols, "name")
	for _, row := range rows {
		name, _ := row[nameIdx].(string)
		ccols, crows, err := db.Rows(ctx, db.GetDB(), `
			select v.code, v.name, v.category, v.quantity, v.min_stock, v.price, v.stock_value
			  from public.v_component_stock v
			 where v.supplier = $1
			 order by v.name`, name)
		if err != nil {
			return err
		}
		if len(crows) == 0 {
			continue
		}
		r.Sections = append(r.Sections, Section{Title: name, Columns: ccols, Rows: crows})
	}

	var total float64
	if err := db.GetDB().QueryRow(ctx,
		`select coalesce(sum(stock_value), 0) from public.v_supplier_spend`).Scan(&total); err != nil {
		return err
	}
	r.Summary = map[string]any{"suppliers": len(rows), "stockValue": round2(total)}
	return nil
}

// ---------------------------------------------------------------------- audit

func audit(ctx context.Context, r *Report, from, to, userID string) error {
	sql := `
		select extract(epoch from a.created_at) * 1000 as ts,
		       a.actor, a.body as event, a.entity_type, a.entity_id
		  from public.activity a
		 where ($1 = '' or a.created_at >= $1::date)
		   and ($2 = '' or a.created_at < ($2::date + 1))
		   and ($3 = '' or a.actor_id = $3::uuid)
		 order by a.created_at desc
		 limit 5000`
	cols, rows, err := db.Rows(ctx, db.GetDB(), sql, from, to, userID)
	if err != nil {
		return err
	}
	r.Sections = []Section{{Title: "Events", Columns: cols, Rows: rows}}

	acols, arows, err := db.Rows(ctx, db.GetDB(), `
		select coalesce(nullif(a.actor, ''), 'system') as actor, count(*) as events,
		       extract(epoch from max(a.created_at)) * 1000 as last_seen
		  from public.activity a
		 where ($1 = '' or a.created_at >= $1::date)
		   and ($2 = '' or a.created_at < ($2::date + 1))
		 group by 1 order by events desc`, from, to)
	if err != nil {
		return err
	}
	r.Sections = append(r.Sections, Section{Title: "By person", Columns: acols, Rows: arows})

	r.Summary = map[string]any{"events": len(rows), "from": from, "to": to}
	return nil
}

// ------------------------------------------------------------------------ CSV

// WriteCSV streams straight to the response — no temporary files.
func WriteCSV(w io.Writer, r *Report) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{r.Name}); err != nil {
		return err
	}
	if err := cw.Write([]string{"Generated", r.GeneratedAt.Format("2006-01-02 15:04")}); err != nil {
		return err
	}

	for _, sec := range r.Sections {
		_ = cw.Write(nil)
		if err := cw.Write([]string{sec.Title}); err != nil {
			return err
		}
		if err := cw.Write(headerRow(sec.Columns)); err != nil {
			return err
		}
		for _, row := range sec.Rows {
			out := make([]string, len(row))
			for i, v := range row {
				out[i] = cell(v)
			}
			if err := cw.Write(out); err != nil {
				return err
			}
		}
		if len(sec.Totals) > 0 {
			line := []string{"Total"}
			for _, k := range sortedKeys(sec.Totals) {
				line = append(line, k+": "+cell(sec.Totals[k]))
			}
			if err := cw.Write(line); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// ------------------------------------------------------------------------ PDF

// WritePDF renders a print-optimised document: white background, black text,
// a header and a page footer. Landscape, because these tables are wide.
func WritePDF(w io.Writer, r *Report) error {
	pdf := gofpdf.New("L", "mm", "A4", "")
	pdf.SetMargins(10, 12, 10)
	pdf.SetAutoPageBreak(true, 15)

	generated := r.GeneratedAt.Format("2 January 2006, 15:04")

	pdf.SetHeaderFunc(func() {
		pdf.SetFont("Helvetica", "B", 13)
		pdf.SetTextColor(0, 0, 0)
		pdf.CellFormat(0, 7, tr(r.Name), "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 8)
		pdf.SetTextColor(90, 90, 90)
		pdf.CellFormat(0, 5, tr("Electronic Store Manager · AskSummu Pvt. Ltd. · "+generated), "", 1, "L", false, 0, "")
		pdf.Ln(2)
		pdf.SetDrawColor(200, 200, 200)
		pdf.Line(10, pdf.GetY(), 287, pdf.GetY())
		pdf.Ln(3)
	})

	pdf.SetFooterFunc(func() {
		pdf.SetY(-12)
		pdf.SetFont("Helvetica", "", 8)
		pdf.SetTextColor(120, 120, 120)
		pdf.CellFormat(0, 6, tr(fmt.Sprintf("Page %d", pdf.PageNo())), "", 0, "C", false, 0, "")
	})

	pdf.AddPage()

	if len(r.Summary) > 0 {
		pdf.SetFont("Helvetica", "", 9)
		pdf.SetTextColor(40, 40, 40)
		var parts []string
		for _, k := range sortedKeys(r.Summary) {
			parts = append(parts, models.Label(k)+": "+cell(r.Summary[k]))
		}
		pdf.MultiCell(0, 5, tr(strings.Join(parts, "   ·   ")), "", "L", false)
		pdf.Ln(2)
	}

	for _, sec := range r.Sections {
		if pdf.GetY() > 175 {
			pdf.AddPage()
		}
		pdf.SetFont("Helvetica", "B", 10)
		pdf.SetTextColor(0, 0, 0)
		pdf.CellFormat(0, 6, tr(sec.Title), "", 1, "L", false, 0, "")

		if len(sec.Rows) == 0 {
			pdf.SetFont("Helvetica", "I", 8)
			pdf.SetTextColor(120, 120, 120)
			pdf.CellFormat(0, 5, tr("Nothing to report."), "", 1, "L", false, 0, "")
			pdf.Ln(2)
			continue
		}

		usable := 277.0
		colW := usable / float64(len(sec.Columns))
		if colW < 16 {
			colW = 16
		}

		pdf.SetFont("Helvetica", "B", 7.5)
		pdf.SetFillColor(240, 240, 240)
		pdf.SetTextColor(30, 30, 30)
		for _, c := range headerRow(sec.Columns) {
			pdf.CellFormat(colW, 6, tr(clip(c, colW)), "1", 0, "L", true, 0, "")
		}
		pdf.Ln(-1)

		pdf.SetFont("Helvetica", "", 7.5)
		pdf.SetTextColor(0, 0, 0)
		for _, row := range sec.Rows {
			if pdf.GetY() > 190 {
				pdf.AddPage()
			}
			for _, v := range row {
				pdf.CellFormat(colW, 5, tr(clip(cell(v), colW)), "1", 0, "L", false, 0, "")
			}
			pdf.Ln(-1)
		}

		if len(sec.Totals) > 0 {
			var parts []string
			for _, k := range sortedKeys(sec.Totals) {
				parts = append(parts, models.Label(k)+": "+cell(sec.Totals[k]))
			}
			pdf.SetFont("Helvetica", "B", 8)
			pdf.CellFormat(0, 6, tr(strings.Join(parts, "   ")), "", 1, "R", false, 0, "")
		}
		pdf.Ln(3)
	}

	return pdf.Output(w)
}

// tr converts UTF-8 to the Latin-1 the standard PDF fonts use. Without it a
// rupee sign or an en dash comes out as noise.
func tr(s string) string {
	s = strings.NewReplacer(
		"₹", "Rs.", "—", "-", "–", "-", "·", "-",
		"’", "'", "‘", "'", "“", `"`, "”", `"`, "→", "->", "≤", "<=", "≥", ">=", "…", "...",
	).Replace(s)
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 256 {
			out = append(out, r)
		} else {
			out = append(out, '?')
		}
	}
	return string(out)
}

// ------------------------------------------------------------------- storage

// Save records a generated report so it can be reopened later.
func Save(ctx context.Context, userID string, r *Report) (string, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	var id string
	err = db.GetDB().QueryRow(ctx, `
		insert into public.reports (user_id, type, name, data)
		values ($1, $2, $3, $4) returning id`,
		nullUUID(userID), r.Type, r.Name, body).Scan(&id)
	return id, err
}

func Load(ctx context.Context, id string) (*Report, error) {
	var body []byte
	if err := db.GetDB().QueryRow(ctx,
		`select data from public.reports where id = $1`, id).Scan(&body); err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ------------------------------------------------------------------- helpers

func currencySymbol(ctx context.Context) (string, error) {
	var s string
	err := db.GetDB().QueryRow(ctx, `select currency_symbol from public.settings where id = 1`).Scan(&s)
	if err != nil {
		return "₹", nil // a missing settings row must not fail a report
	}
	return s, nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func headerRow(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = models.Label(strings.ReplaceAll(c, "_", " "))
	}
	return out
}

func indexOf(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return 0
}

func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int, int32, int64:
		return fmt.Sprintf("%d", t)
	case time.Time:
		return t.Format("2006-01-02 15:04")
	case []byte:
		return string(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		s := string(b)
		return strings.Trim(s, `"`)
	}
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(string(t), 64)
		return f
	}
	// pgx returns numeric as a type with a String method
	if s, ok := v.(fmt.Stringer); ok {
		f, _ := strconv.ParseFloat(s.String(), 64)
		return f
	}
	return 0
}

func toInt(v any) int64 { return int64(toFloat(v)) }

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

func clip(s string, widthMM float64) string {
	max := int(widthMM / 1.55)
	if max < 4 {
		max = 4
	}
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

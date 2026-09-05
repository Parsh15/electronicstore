package models

import "strings"

// Settings is the single store-wide row.
type Settings struct {
	CurrencySymbol  string     `json:"currencySymbol"`
	LowStockDefault int        `json:"lowStockDefault"`
	CompPrefix      string     `json:"compPrefix"`
	BoxPrefix       string     `json:"boxPrefix"`
	DateFmt         string     `json:"dateFmt"`
	Automation      Automation `json:"automation"`
}

const SettingsColumns = `s.currency_symbol as "currencySymbol",
	s.low_stock_default as "lowStockDefault",
	s.comp_prefix as "compPrefix", s.box_prefix as "boxPrefix",
	s.date_fmt as "dateFmt", s.automation`

// Automation is what the automation engine consults before acting. Defaults
// match the values the existing UI ships with, so behaviour is unchanged.
type Automation struct {
	Labels      *bool `json:"labels"`
	Bins        *bool `json:"bins"`
	Reorder     *bool `json:"reorder"`
	Units       *bool `json:"units"`
	BoxCapacity *int  `json:"boxCapacity"`
}

func (a Automation) LabelsOn() bool  { return a.Labels == nil || *a.Labels }
func (a Automation) BinsOn() bool    { return a.Bins == nil || *a.Bins }
func (a Automation) ReorderOn() bool { return a.Reorder == nil || *a.Reorder }
func (a Automation) UnitsOn() bool   { return a.Units == nil || *a.Units }

func (a Automation) Capacity() int {
	if a.BoxCapacity != nil && *a.BoxCapacity > 0 {
		return *a.BoxCapacity
	}
	return 250
}

// SettingsRequest is a patch — only the fields present are written.
type SettingsRequest struct {
	CurrencySymbol  *string `json:"currencySymbol"`
	LowStockDefault *int    `json:"lowStockDefault"`
	CompPrefix      *string `json:"compPrefix"`
	BoxPrefix       *string `json:"boxPrefix"`
	DateFmt         *string `json:"dateFmt"`
}

func (r *SettingsRequest) Validate() *Problem {
	Trim(r.CurrencySymbol)
	Trim(r.CompPrefix)
	Trim(r.BoxPrefix)

	if r.CurrencySymbol != nil {
		if p := First(Required("currencySymbol", *r.CurrencySymbol),
			MaxLen("currencySymbol", *r.CurrencySymbol, 4)); p != nil {
			return p
		}
	}
	if r.LowStockDefault != nil {
		if p := InRange("lowStockDefault", *r.LowStockDefault, 0, 1_000_000); p != nil {
			return p
		}
	}
	for field, v := range map[string]*string{"compPrefix": r.CompPrefix, "boxPrefix": r.BoxPrefix} {
		if v == nil {
			continue
		}
		*v = strings.ToUpper(*v)
		if p := First(Required(field, *v), MaxLen(field, *v, 8)); p != nil {
			return p
		}
		for _, ch := range *v {
			if !(ch >= 'A' && ch <= 'Z') && !(ch >= '0' && ch <= '9') {
				return Invalid(field, "Use letters and numbers only.")
			}
		}
	}
	if r.DateFmt != nil {
		if p := OneOf("dateFmt", *r.DateFmt, "short", "medium", "long"); p != nil {
			return p
		}
	}
	return nil
}

// AutomationRequest patches the automation flags.
type AutomationRequest struct {
	Labels      *bool `json:"labels"`
	Bins        *bool `json:"bins"`
	Reorder     *bool `json:"reorder"`
	Units       *bool `json:"units"`
	BoxCapacity *int  `json:"boxCapacity"`
}

func (r *AutomationRequest) Validate() *Problem {
	if r.BoxCapacity != nil {
		return InRange("boxCapacity", *r.BoxCapacity, 1, 1_000_000)
	}
	return nil
}

// ReportRequest — POST /api/reports/generate
type ReportRequest struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	ProjectID string `json:"projectId"`
	From      string `json:"from"`
	To        string `json:"to"`
	UserID    string `json:"userId"`
	Save      bool   `json:"save"`
	Format    string `json:"format"` // json (default) | csv | pdf
}

var ReportTypes = []string{"inventory", "low-stock", "valuation", "bom", "supplier", "audit"}

func (r *ReportRequest) Validate() *Problem {
	r.Type = strings.TrimSpace(strings.ToLower(r.Type))
	if r.Format == "" {
		r.Format = "json"
	}
	if p := First(
		OneOf("type", r.Type, ReportTypes...),
		OneOf("format", r.Format, "json", "csv", "pdf"),
		MaxLen("name", r.Name, 200),
	); p != nil {
		return p
	}
	if r.ProjectID != "" && !IsUUID(r.ProjectID) {
		return Invalid("projectId", "That project is not valid.")
	}
	if r.UserID != "" && !IsUUID(r.UserID) {
		return Invalid("userId", "That user is not valid.")
	}
	for field, d := range map[string]string{"from": r.From, "to": r.To} {
		if d != "" && len(d) != 10 {
			return Invalid(field, "Use the date format YYYY-MM-DD.")
		}
	}
	return nil
}

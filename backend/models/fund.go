package models

import "strings"

type Fund struct {
	ID        string   `json:"id"`
	Code      string   `json:"code"`
	Name      string   `json:"name"`
	Provider  string   `json:"provider"`
	Kind      string   `json:"kind"`
	Currency  string   `json:"currency"`
	EventID   *string  `json:"eventId"`
	Requested *float64 `json:"requested"`
	Approved  *float64 `json:"approved"`
	Received  *float64 `json:"received"`
	AppliedOn string   `json:"appliedOn"`
	Deadline  string   `json:"deadline"`
	Status    string   `json:"status"`
	Contact   string   `json:"contact"`
	Ref       string   `json:"ref"`
	Notes     string   `json:"notes"`
	Docs      string   `json:"docs"`
}

const FundColumns = `f.id, f.code, f.name, f.provider, f.kind, f.currency,
	f.event_id as "eventId", f.requested, f.approved, f.received,
	coalesce(f.applied_on::text, '') as "appliedOn",
	coalesce(f.deadline::text, '')   as deadline,
	f.status, f.contact, f.ref, f.notes, f.docs,
	extract(epoch from f.created_at) * 1000 as created`

var (
	FundKinds    = []string{"Grant", "Competition Prize", "Sponsorship", "Internal Budget", "Loan", "Other"}
	FundStatuses = []string{"Draft", "Applied", "Under Review", "Approved", "Received", "Rejected", "Closed"}
	Currencies   = []string{"INR", "USD"}
)

type FundRequest struct {
	Name       string    `json:"name"`
	Provider   string    `json:"provider"`
	Kind       string    `json:"kind"`
	Currency   string    `json:"currency"`
	EventID    *string   `json:"eventId"`
	Requested  *float64  `json:"requested"`
	Approved   *float64  `json:"approved"`
	Received   *float64  `json:"received"`
	AppliedOn  string    `json:"appliedOn"`
	Deadline   string    `json:"deadline"`
	Status     string    `json:"status"`
	Contact    string    `json:"contact"`
	Ref        string    `json:"ref"`
	Notes      string    `json:"notes"`
	Docs       string    `json:"docs"`
	ProjectIDs []string  `json:"projectIds"`
	Parts      []BOMLine `json:"parts"`
}

func (r *FundRequest) Validate() *Problem {
	r.Name = strings.TrimSpace(r.Name)
	r.Provider = strings.TrimSpace(r.Provider)
	if r.Kind == "" {
		r.Kind = "Grant"
	}
	if r.Currency == "" {
		r.Currency = "INR"
	}
	if r.Status == "" {
		r.Status = "Draft"
	}

	if p := First(
		Required("name", r.Name),
		MaxLen("name", r.Name, 200),
		MaxLen("provider", r.Provider, 200),
		MaxLen("contact", r.Contact, 200),
		MaxLen("ref", r.Ref, 120),
		MaxLen("notes", r.Notes, 8000),
		MaxLen("docs", r.Docs, 4000),
		OneOf("kind", r.Kind, FundKinds...),
		OneOf("currency", r.Currency, Currencies...),
		OneOf("status", r.Status, FundStatuses...),
	); p != nil {
		return p
	}

	for field, v := range map[string]*float64{
		"requested": r.Requested, "approved": r.Approved, "received": r.Received,
	} {
		if v != nil && (*v < 0 || *v > 1e12) {
			return Invalid(field, Label(field)+" must be zero or more.")
		}
	}
	if r.EventID != nil && *r.EventID != "" && !IsUUID(*r.EventID) {
		return Invalid("eventId", "That event is not valid.")
	}
	for _, d := range map[string]string{"appliedOn": r.AppliedOn, "deadline": r.Deadline} {
		if d != "" && len(d) != 10 {
			return Invalid("deadline", "Use the date format YYYY-MM-DD.")
		}
	}
	for _, id := range r.ProjectIDs {
		if !IsUUID(id) {
			return Invalid("projectIds", "One of the linked projects is not valid.")
		}
	}
	for i := range r.Parts {
		if p := r.Parts[i].Validate(); p != nil {
			return p
		}
	}
	return nil
}

// AdvanceFundRequest — POST /api/funds/:id/advance
type AdvanceFundRequest struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

func (r *AdvanceFundRequest) Validate() *Problem {
	return First(
		OneOf("status", r.Status, FundStatuses...),
		MaxLen("note", r.Note, 2000),
	)
}

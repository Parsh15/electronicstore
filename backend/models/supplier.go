package models

import "strings"

type Supplier struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Contact string `json:"contact"`
	Email   string `json:"email"`
	Phone   string `json:"phone"`
	Website string `json:"website"`
	Notes   string `json:"notes"`
}

const SupplierColumns = `s.id, s.name, s.contact, s.email, s.phone, s.website, s.notes,
	extract(epoch from s.created_at) * 1000 as created`

type SupplierRequest struct {
	Name    string `json:"name"`
	Contact string `json:"contact"`
	Email   string `json:"email"`
	Phone   string `json:"phone"`
	Website string `json:"website"`
	Notes   string `json:"notes"`
}

func (r *SupplierRequest) Validate() *Problem {
	r.Name = strings.TrimSpace(r.Name)
	r.Email = strings.TrimSpace(r.Email)
	if p := First(
		Required("name", r.Name),
		MaxLen("name", r.Name, 160),
		MaxLen("contact", r.Contact, 160),
		MaxLen("phone", r.Phone, 60),
		MaxLen("website", r.Website, 300),
		MaxLen("notes", r.Notes, 4000),
	); p != nil {
		return p
	}
	// Email is optional here — plenty of suppliers are just a name and a phone
	// number — but when present it must be well formed.
	if r.Email != "" {
		return ValidEmail("email", r.Email)
	}
	return nil
}

package models

import "strings"

type Event struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Org   string `json:"org"`
	Type  string `json:"type"`
	Date  string `json:"date"`
	Venue string `json:"venue"`
	Notes string `json:"notes"`
}

const EventColumns = `e.id, e.name, e.org, e.type,
	coalesce(e.event_date::text, '') as date, e.venue, e.notes,
	extract(epoch from e.created_at) * 1000 as created`

var EventTypes = []string{"Competition", "Grant Cycle", "Exhibition", "Hackathon", "Conference", "Other"}

type EventRequest struct {
	Name  string `json:"name"`
	Org   string `json:"org"`
	Type  string `json:"type"`
	Date  string `json:"date"`
	Venue string `json:"venue"`
	Notes string `json:"notes"`
}

func (r *EventRequest) Validate() *Problem {
	r.Name = strings.TrimSpace(r.Name)
	r.Org = strings.TrimSpace(r.Org)
	if r.Type == "" {
		r.Type = "Competition"
	}
	if p := First(
		Required("name", r.Name),
		MaxLen("name", r.Name, 200),
		MaxLen("org", r.Org, 200),
		MaxLen("venue", r.Venue, 200),
		MaxLen("notes", r.Notes, 4000),
		OneOf("type", r.Type, EventTypes...),
	); p != nil {
		return p
	}
	if r.Date != "" && len(r.Date) != 10 {
		return Invalid("date", "Use the date format YYYY-MM-DD.")
	}
	return nil
}

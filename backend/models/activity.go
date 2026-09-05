package models

import "strings"

// Activity is the audit trail. Rows are written by the stored functions and by
// the Go services; the API only ever appends or reads.
type Activity struct {
	ID         string `json:"id"`
	Body       string `json:"text"` // the existing UI reads "text"
	Glyph      string `json:"glyph"`
	Color      string `json:"color"`
	Actor      string `json:"actor"`
	EntityType string `json:"entityType"`
	EntityID   string `json:"entityId"`
	CreatedAt  int64  `json:"ts"`
}

const ActivityColumns = `a.id, a.body as text, a.glyph, a.color, a.actor,
	a.entity_type as "entityType", a.entity_id as "entityId",
	extract(epoch from a.created_at) * 1000 as ts`

type ActivityRequest struct {
	Body       string `json:"body"`
	Text       string `json:"text"`
	Glyph      string `json:"glyph"`
	Color      string `json:"color"`
	EntityType string `json:"entityType"`
	EntityID   string `json:"entityId"`
}

func (r *ActivityRequest) Validate() *Problem {
	if r.Body == "" {
		r.Body = r.Text
	}
	r.Body = strings.TrimSpace(r.Body)
	if r.Glyph == "" {
		r.Glyph = "•"
	}
	if r.Color == "" {
		r.Color = "#8da2c8"
	}
	return First(
		Required("body", r.Body),
		MaxLen("body", r.Body, 1000),
		MaxLen("glyph", r.Glyph, 8),
		MaxLen("color", r.Color, 32),
		MaxLen("entityType", r.EntityType, 40),
		MaxLen("entityId", r.EntityID, 80),
	)
}

// VoiceLogRequest — POST /api/voice/log. Recognition happens in the browser;
// this is analytics only.
type VoiceLogRequest struct {
	Command string `json:"command"`
	Action  string `json:"action"`
	Success *bool  `json:"success"`
}

func (r *VoiceLogRequest) Validate() *Problem {
	r.Command = strings.TrimSpace(r.Command)
	return First(
		Required("command", r.Command),
		MaxLen("command", r.Command, 500),
		MaxLen("action", r.Action, 120),
	)
}

// TrashEntry is a soft-deleted row plus enough context to describe it.
type TrashEntry struct {
	TID       string `json:"tid"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Payload   any    `json:"obj"`
	DeletedBy string `json:"by"`
	DeletedAt int64  `json:"at"`
}

const TrashColumns = `t.tid, t.kind, t.label, t.payload as obj,
	t.deleted_by as by, extract(epoch from t.deleted_at) * 1000 as at`

// SoftDeleteKinds mirrors the guard inside soft_delete().
var SoftDeleteKinds = []string{"components", "projects", "boxes", "funds", "events", "suppliers", "labels"}

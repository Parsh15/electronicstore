package models

import "strings"

type Project struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Detail      string `json:"detail"`
	FileName    string `json:"fileName"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created"`
}

const ProjectColumns = `p.id, p.code, p.name, p.description, p.detail,
	p.file_name as "fileName", p.status,
	extract(epoch from p.started_at) * 1000   as "startedAt",
	extract(epoch from p.completed_at) * 1000 as "completedAt",
	extract(epoch from p.created_at) * 1000   as created`

var ProjectStatuses = []string{"planned", "active", "on-hold", "complete", "cancelled"}

type ProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Detail      string `json:"detail"`
	FileName    string `json:"fileName"`
	Status      string `json:"status"`
}

func (r *ProjectRequest) Validate() *Problem {
	r.Name = strings.TrimSpace(r.Name)
	r.Description = strings.TrimSpace(r.Description)
	if r.Status == "" {
		r.Status = "planned"
	}
	return First(
		Required("name", r.Name),
		MaxLen("name", r.Name, 200),
		MaxLen("description", r.Description, 1000),
		MaxLen("detail", r.Detail, 20000),
		MaxLen("fileName", r.FileName, 300),
		OneOf("status", r.Status, ProjectStatuses...),
	)
}

// BOMLine is one bill-of-materials row.
type BOMLine struct {
	ComponentID string `json:"componentId"`
	ID          string `json:"id"` // the existing UI sends the component id as "id"
	Qty         int    `json:"qty"`
	Status      string `json:"status"`
}

func (r *BOMLine) Validate() *Problem {
	if r.ComponentID == "" {
		r.ComponentID = r.ID
	}
	if r.Status == "" {
		r.Status = "planned"
	}
	return First(
		ValidUUID("componentId", r.ComponentID),
		InRange("qty", r.Qty, 1, 1_000_000),
		OneOf("status", r.Status, "planned", "reserved", "taken", "returned"),
	)
}

// BOMRequest replaces or adds lines. The UI sends the whole list when editing
// a project, and a single line when adding from the component page — both
// shapes are accepted.
type BOMRequest struct {
	Parts []BOMLine `json:"parts"`
	BOMLine
	// Replace = true means "this is the whole BOM now"; false appends.
	Replace bool `json:"replace"`
}

func (r *BOMRequest) Validate() *Problem {
	if len(r.Parts) == 0 && r.ComponentID == "" && r.ID == "" {
		return Invalid("parts", "Add at least one component to the BOM.")
	}
	if len(r.Parts) == 0 {
		if p := r.BOMLine.Validate(); p != nil {
			return p
		}
		r.Parts = []BOMLine{r.BOMLine}
		return nil
	}
	if len(r.Parts) > 2000 {
		return Invalid("parts", "A BOM can hold at most 2000 lines.")
	}
	seen := map[string]bool{}
	for i := range r.Parts {
		if p := r.Parts[i].Validate(); p != nil {
			return p
		}
		if seen[r.Parts[i].ComponentID] {
			return Invalid("parts", "The same component is listed twice.")
		}
		seen[r.Parts[i].ComponentID] = true
	}
	return nil
}

// BOMUpdateRequest edits one existing line.
type BOMUpdateRequest struct {
	Qty    *int    `json:"qty"`
	Status *string `json:"status"`
}

func (r *BOMUpdateRequest) Validate() *Problem {
	if r.Qty == nil && r.Status == nil {
		return Invalid("", "Nothing to change.")
	}
	if r.Qty != nil {
		if p := InRange("qty", *r.Qty, 1, 1_000_000); p != nil {
			return p
		}
	}
	if r.Status != nil {
		if p := OneOf("status", *r.Status, "planned", "reserved", "taken", "returned"); p != nil {
			return p
		}
	}
	return nil
}

package models

import "strings"

type Box struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Location    string `json:"location"`
	Description string `json:"description"`
	Capacity    int    `json:"capacity"`
	ImageName   string `json:"imageName"`
}

const BoxColumns = `b.id, b.code, b.name, b.location, b.description, b.capacity,
	b.image_name as "imageName", b.image_url as "imageUrl",
	extract(epoch from b.created_at) * 1000 as created`

type BoxRequest struct {
	Name        string `json:"name"`
	Location    string `json:"location"`
	Description string `json:"description"`
	Capacity    int    `json:"capacity"`
	ImageName   string `json:"imageName"`
}

func (r *BoxRequest) Validate() *Problem {
	r.Name = strings.TrimSpace(r.Name)
	r.Location = strings.TrimSpace(r.Location)
	if r.Capacity == 0 {
		r.Capacity = 250 // matches the settings default
	}
	return First(
		Required("name", r.Name),
		MaxLen("name", r.Name, 160),
		MaxLen("location", r.Location, 120),
		MaxLen("description", r.Description, 2000),
		InRange("capacity", r.Capacity, 1, 1_000_000),
	)
}

// BoxAssignRequest puts a quantity of a component into a box.
type BoxAssignRequest struct {
	ComponentID string `json:"componentId"`
	Qty         int    `json:"qty"`
	// Remove = true takes the component out of the box instead.
	Remove bool `json:"remove"`
}

func (r *BoxAssignRequest) Validate() *Problem {
	if p := ValidUUID("componentId", r.ComponentID); p != nil {
		return p
	}
	if r.Remove {
		return nil
	}
	return InRange("qty", r.Qty, 1, 10_000_000)
}

// LabelRequest — POST /api/labels/generate
type LabelRequest struct {
	Type        string `json:"type"`
	ComponentID string `json:"componentId"`
	UnitID      string `json:"unitId"`
	BoxID       string `json:"boxId"`
	Qty         int    `json:"qty"`
	Size        string `json:"size"`
}

var LabelSizes = []string{"small", "medium", "large", "38x19", "50x25", "70x38"}

func (r *LabelRequest) Validate() *Problem {
	if r.Type == "" {
		r.Type = "component"
	}
	if r.Size == "" {
		r.Size = "medium"
	}
	if r.Qty == 0 {
		r.Qty = 1
	}
	if p := First(
		OneOf("type", r.Type, "component", "unit", "box", "batch"),
		OneOf("size", r.Size, LabelSizes...),
		InRange("qty", r.Qty, 1, 2000),
	); p != nil {
		return p
	}

	switch r.Type {
	case "component", "batch":
		return ValidUUID("componentId", r.ComponentID)
	case "unit":
		return ValidUUID("unitId", r.UnitID)
	case "box":
		return ValidUUID("boxId", r.BoxID)
	}
	return nil
}

// PrintQueueRequest marks labels as printed.
type PrintQueueRequest struct {
	LabelIDs []string `json:"labelIds"`
	Printed  bool     `json:"printed"`
}

func (r *PrintQueueRequest) Validate() *Problem {
	if len(r.LabelIDs) == 0 {
		return Invalid("labelIds", "Select at least one label.")
	}
	if len(r.LabelIDs) > 5000 {
		return Invalid("labelIds", "Queue at most 5000 labels at a time.")
	}
	return nil
}

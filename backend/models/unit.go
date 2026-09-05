package models

import "strings"

// Unit is one individually tracked physical item.
type Unit struct {
	ID          string `json:"id"`
	ComponentID string `json:"componentId"`
	UnitID      string `json:"unitId"`
	Status      string `json:"status"`
	Faulty      bool   `json:"faulty"`
	ProjectID   *string `json:"projectId"`
	Batch       bool   `json:"batch"`
	BatchQty    *int   `json:"batchQty"`
	Auto        bool   `json:"auto"`
	CreatedAt   int64  `json:"ts"`
}

const UnitColumns = `u.id, u.component_id as "componentId", u.unit_id as "unitId",
	u.status, u.faulty, u.project_id as "projectId",
	u.batch, u.batch_qty as "batchQty", u.auto,
	extract(epoch from u.created_at) * 1000 as ts`

// UnitStatuses is the single source of truth, mirroring the CHECK constraint.
var UnitStatuses = []string{"stock", "reserved", "in-use", "faulty", "retired"}

type UnitStatusRequest struct {
	Status    string  `json:"status"`
	ProjectID *string `json:"projectId"`
	Note      string  `json:"note"`
}

func (r *UnitStatusRequest) Validate() *Problem {
	r.Status = strings.TrimSpace(r.Status)
	if p := OneOf("status", r.Status, UnitStatuses...); p != nil {
		return p
	}
	if r.Status == "in-use" || r.Status == "reserved" {
		if r.ProjectID == nil || *r.ProjectID == "" {
			return Invalid("projectId", "Choose which project the unit is going to.")
		}
		if !IsUUID(*r.ProjectID) {
			return Invalid("projectId", "That project is not valid.")
		}
	} else {
		r.ProjectID = nil // a stock, faulty or retired unit holds no project
	}
	return MaxLen("note", r.Note, 2000)
}

// UnitUpdateRequest is the editable part of a unit that is not its status.
type UnitUpdateRequest struct {
	UnitID   *string `json:"unitId"`
	Batch    *bool   `json:"batch"`
	BatchQty *int    `json:"batchQty"`
}

func (r *UnitUpdateRequest) Validate() *Problem {
	Trim(r.UnitID)
	if r.UnitID != nil {
		if p := First(Required("unitId", *r.UnitID), MaxLen("unitId", *r.UnitID, 60)); p != nil {
			return p
		}
	}
	if r.BatchQty != nil && (*r.BatchQty < 1 || *r.BatchQty > 1_000_000) {
		return Invalid("batchQty", "Batch quantity must be at least 1.")
	}
	return nil
}

// BulkStatusRequest moves many units at once — the bulk actions in the units
// table. Runs as one transaction.
type BulkStatusRequest struct {
	UnitIDs   []string `json:"unitIds"`
	Status    string   `json:"status"`
	ProjectID *string  `json:"projectId"`
}

func (r *BulkStatusRequest) Validate() *Problem {
	if len(r.UnitIDs) == 0 {
		return Invalid("unitIds", "Select at least one unit.")
	}
	if len(r.UnitIDs) > 2000 {
		return Invalid("unitIds", "Change at most 2000 units at a time.")
	}
	one := UnitStatusRequest{Status: r.Status, ProjectID: r.ProjectID}
	if p := one.Validate(); p != nil {
		return p
	}
	r.ProjectID = one.ProjectID
	return nil
}

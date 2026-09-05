package models

import "strings"

// Component is the stored shape. Field names match the SQL columns; the JSON
// tags match what the existing frontend already reads, so no UI code changes.
type Component struct {
	ID           string   `json:"id"`
	Code         string   `json:"code"`
	Name         string   `json:"name"`
	Category     string   `json:"category"`
	Location     string   `json:"location"`
	Quantity     int      `json:"quantity"`
	MinStock     *int     `json:"minStock"`
	ReorderPoint *int     `json:"reorderPoint"`
	Price        *float64 `json:"price"`
	SupplierID   *string  `json:"supplierId"`
	Supplier     string   `json:"supplier"`
	Faulty       bool     `json:"faulty"`
	UnitTracked  bool     `json:"unitTracked"`
	ImageName    string   `json:"imageName"`
	Expiry       string   `json:"expiry"`
	Datasheet    string   `json:"datasheet"`
	Notes        string   `json:"notes"`
	CreatedAt    int64    `json:"created"`
}

// ComponentColumns is the canonical select list, aliased into the JSON names
// the frontend expects.
const ComponentColumns = `c.id, c.code, c.name, c.category, c.location, c.quantity,
	c.min_stock as "minStock", c.reorder_point as "reorderPoint", c.price,
	c.supplier_id as "supplierId",
	coalesce(nullif(c.supplier_name, ''), s.name, '') as supplier,
	c.faulty, c.unit_tracked as "unitTracked",
	c.image_name as "imageName", c.image_url as "imageUrl",
	coalesce(c.expiry::text, '') as expiry, c.datasheet, c.notes,
	extract(epoch from c.created_at) * 1000 as created`

type ComponentRequest struct {
	Name         string   `json:"name"`
	Code         string   `json:"code"`
	Category     string   `json:"category"`
	Location     string   `json:"location"`
	Quantity     int      `json:"quantity"`
	MinStock     *int     `json:"minStock"`
	ReorderPoint *int     `json:"reorderPoint"`
	Price        *float64 `json:"price"`
	SupplierID   *string  `json:"supplierId"`
	Supplier     string   `json:"supplier"`
	UnitTracked  bool     `json:"unitTracked"`
	Faulty       bool     `json:"faulty"`
	ImageName    string   `json:"imageName"`
	Expiry       string   `json:"expiry"`
	Datasheet    string   `json:"datasheet"`
	Notes        string   `json:"notes"`

	// Automation is opt-out per request; the store-wide defaults live in
	// settings.automation.
	SkipAutomation bool `json:"skipAutomation"`
}

func (r *ComponentRequest) Validate() *Problem {
	r.Name = strings.TrimSpace(r.Name)
	r.Category = strings.TrimSpace(r.Category)
	r.Location = strings.TrimSpace(r.Location)
	r.Supplier = strings.TrimSpace(r.Supplier)
	if r.Category == "" {
		r.Category = "Uncategorised"
	}

	if p := First(
		Required("name", r.Name),
		MaxLen("name", r.Name, 200),
		MaxLen("category", r.Category, 80),
		MaxLen("location", r.Location, 80),
		MaxLen("notes", r.Notes, 4000),
		InRange("quantity", r.Quantity, 0, 10_000_000),
	); p != nil {
		return p
	}
	if r.MinStock != nil && (*r.MinStock < 0 || *r.MinStock > 10_000_000) {
		return Invalid("minStock", "Minimum stock must be zero or more.")
	}
	if r.ReorderPoint != nil && *r.ReorderPoint < 0 {
		return Invalid("reorderPoint", "Reorder point must be zero or more.")
	}
	if r.Price != nil && (*r.Price < 0 || *r.Price > 100_000_000) {
		return Invalid("price", "Enter a price of zero or more.")
	}
	if r.SupplierID != nil && *r.SupplierID != "" && !IsUUID(*r.SupplierID) {
		return Invalid("supplierId", "That supplier is not valid.")
	}
	if r.Expiry != "" && len(r.Expiry) != 10 {
		return Invalid("expiry", "Use the date format YYYY-MM-DD.")
	}
	return nil
}

// RestockRequest — POST /api/components/:id/restock
type RestockRequest struct {
	Add  int    `json:"add"`
	Note string `json:"note"`
}

func (r *RestockRequest) Validate() *Problem {
	if r.Add == 0 {
		return Invalid("add", "Enter how many to add or remove.")
	}
	if r.Add < -1_000_000 || r.Add > 1_000_000 {
		return Invalid("add", "That quantity is out of range.")
	}
	return MaxLen("note", r.Note, 2000)
}

// ImportRow is one line of a Smart Import. Everything but the name is optional,
// matching what the existing importer accepts.
type ImportRow struct {
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Location    string   `json:"location"`
	Quantity    int      `json:"quantity"`
	MinStock    *int     `json:"minStock"`
	Price       *float64 `json:"price"`
	Supplier    string   `json:"supplier"`
	UnitTracked bool     `json:"unitTracked"`
	Notes       string   `json:"notes"`
}

type ImportRequest struct {
	Rows           []ImportRow `json:"rows"`
	SkipAutomation bool        `json:"skipAutomation"`
	// Merge updates the quantity of an existing component with the same name
	// instead of creating a duplicate.
	Merge bool `json:"merge"`
}

// Validate checks every row before a single insert runs, so an import either
// lands whole or not at all — and the error names the offending line.
func (r *ImportRequest) Validate() *Problem {
	if len(r.Rows) == 0 {
		return Invalid("rows", "There was nothing to import.")
	}
	if len(r.Rows) > 5000 {
		return Invalid("rows", "Import at most 5000 rows at a time.")
	}
	for i := range r.Rows {
		row := &r.Rows[i]
		row.Name = strings.TrimSpace(row.Name)
		row.Category = strings.TrimSpace(row.Category)
		if row.Category == "" {
			row.Category = "Uncategorised"
		}
		if row.Name == "" {
			return Invalid("rows", rowLabel(i)+" has no name.")
		}
		if len([]rune(row.Name)) > 200 {
			return Invalid("rows", rowLabel(i)+" has a name longer than 200 characters.")
		}
		if row.Quantity < 0 || row.Quantity > 10_000_000 {
			return Invalid("rows", rowLabel(i)+" has an out-of-range quantity.")
		}
		if row.Price != nil && *row.Price < 0 {
			return Invalid("rows", rowLabel(i)+" has a negative price.")
		}
		if row.MinStock != nil && *row.MinStock < 0 {
			return Invalid("rows", rowLabel(i)+" has a negative minimum stock.")
		}
	}
	return nil
}

func rowLabel(i int) string {
	return "Row " + itoa(i+1)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// CommentRequest is shared by every entity that accepts notes.
type CommentRequest struct {
	Body   string `json:"body"`
	Text   string `json:"text"` // the existing UI sends "text"
	Tag    string `json:"tag"`
	Author string `json:"author"`
}

func (r *CommentRequest) Validate() *Problem {
	if r.Body == "" {
		r.Body = r.Text
	}
	r.Body = strings.TrimSpace(r.Body)
	if r.Tag == "" {
		r.Tag = "General"
	}
	return First(
		Required("body", r.Body),
		MaxLen("body", r.Body, 4000),
		OneOf("tag", r.Tag, "General", "Faulty Note", "Restock Note", "Build Note", "Funding Note"),
	)
}

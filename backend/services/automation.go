package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

// The automation engine, moved server-side.
//
// It used to run in the browser after a quantity was committed, which meant two
// clients saving at once could each assign the same bin or mint the same label
// range. Here it runs inside the same transaction as the quantity write, so the
// label rows, the bin assignment, the reorder point and the unit rows are
// either all there or none of them are.
//
// The frontend contract is unchanged: it gets back a plan summary and shows it
// as one muted line.

type Plan struct {
	LabelsQueued   int      `json:"labels_queued"`
	BinAssigned    string   `json:"bin_assigned"`
	ReorderPoint   int      `json:"reorder_point"`
	UnitsCreated   int      `json:"units_created"`
	AutomationsRan int      `json:"automations_ran"`
	Notes          []string `json:"notes,omitempty"`
}

// LabelGroupSize implements the division rules exactly as specified: small
// quantities get a label each, larger ones get grouped so a 2000-piece reel
// does not produce 2000 labels.
func LabelGroupSize(qty int) int {
	switch {
	case qty <= 10:
		return 1
	case qty <= 50:
		return 5
	case qty <= 200:
		return 10
	case qty <= 500:
		return 25
	default:
		return 50
	}
}

// LabelCount is how many labels a quantity produces.
func LabelCount(qty int) int {
	if qty <= 0 {
		return 0
	}
	g := LabelGroupSize(qty)
	return int(math.Ceil(float64(qty) / float64(g)))
}

// ReorderPoint: a fifth of the quantity, rounded up to the nearest five, so
// the number that lands in the field is one a person would have chosen.
func ReorderPoint(qty int) int {
	if qty <= 0 {
		return 0
	}
	return int(math.Ceil(float64(qty)*0.20/5.0)) * 5
}

// LoadAutomation reads the store-wide flags.
func LoadAutomation(ctx context.Context, q db.Querier) (models.Automation, error) {
	var raw []byte
	var a models.Automation
	if err := q.QueryRow(ctx, `select automation from public.settings where id = 1`).Scan(&raw); err != nil {
		return a, err
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &a)
	}
	return a, nil
}

// AutoRun is the whole engine. It must be called inside a transaction: the
// caller has usually just written the quantity, and the two belong together.
func AutoRun(ctx context.Context, tx pgx.Tx, componentID string, qty int,
	auto models.Automation, actorID string) (*Plan, error) {

	plan := &Plan{}

	var (
		code, name, category, location string
		unitTracked                   bool
	)
	if err := tx.QueryRow(ctx, `
		select code, name, category, coalesce(location, ''), unit_tracked
		  from public.components where id = $1`, componentID).
		Scan(&code, &name, &category, &location, &unitTracked); err != nil {
		return nil, err
	}

	// ---- 1 + 2. labels ----------------------------------------------------
	if auto.LabelsOn() && qty > 0 {
		group := LabelGroupSize(qty)
		count := LabelCount(qty)

		for i := 0; i < count; i++ {
			labelID := fmt.Sprintf("%s-L%03d", code, i+1)
			inGroup := group
			if rest := qty - i*group; rest < group {
				inGroup = rest
			}
			payload := map[string]any{
				"labelId":   labelID,
				"type":      "component",
				"code":      code,
				"name":      name,
				"category":  category,
				"location":  location,
				"groupSize": inGroup,
				"index":     i + 1,
				"of":        count,
				"size":      "medium",
				"qrContent": qrComponent(componentID, location),
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			tag, err := tx.Exec(ctx, `
				insert into public.labels (label_id, type, component_id, data, created_by)
				values ($1, 'component', $2, $3, $4)
				on conflict (label_id) do nothing`,
				labelID, componentID, body, nullUUID(actorID))
			if err != nil {
				return nil, err
			}
			plan.LabelsQueued += int(tag.RowsAffected())
		}
		if plan.LabelsQueued > 0 {
			plan.AutomationsRan++
			plan.Notes = append(plan.Notes,
				fmt.Sprintf("%d label(s) queued in groups of %d", plan.LabelsQueued, group))
		}
	}

	// ---- 3. bin assignment ------------------------------------------------
	if auto.BinsOn() && qty > 0 {
		bin, err := assignBin(ctx, tx, componentID, category, qty, auto.Capacity())
		if err != nil {
			return nil, err
		}
		if bin != "" {
			plan.BinAssigned = bin
			plan.AutomationsRan++
			plan.Notes = append(plan.Notes, "stored in "+bin)
		}
	}

	// ---- 4. reorder threshold ---------------------------------------------
	if auto.ReorderOn() && qty > 0 {
		rp := ReorderPoint(qty)
		if _, err := tx.Exec(ctx, `
			update public.components
			   set reorder_point = $2,
			       min_stock = coalesce(min_stock, $2)
			 where id = $1`, componentID, rp); err != nil {
			return nil, err
		}
		plan.ReorderPoint = rp
		plan.AutomationsRan++
		plan.Notes = append(plan.Notes, fmt.Sprintf("reorder point set to %d", rp))
	}

	// ---- 5. unit records --------------------------------------------------
	if auto.UnitsOn() && unitTracked && qty > 0 {
		created, err := createUnits(ctx, tx, componentID, code, qty)
		if err != nil {
			return nil, err
		}
		plan.UnitsCreated = created
		if created > 0 {
			plan.AutomationsRan++
			if qty > 50 {
				plan.Notes = append(plan.Notes, fmt.Sprintf("1 batch record for %d units", qty))
			} else {
				plan.Notes = append(plan.Notes, fmt.Sprintf("%d unit record(s) created", created))
			}
		}
	}

	// ---- 6 + 7. dashboard counters and the audit line ---------------------
	// Counters are views (v_component_stock and friends), so they are already
	// current — there is nothing to recompute, only to record.
	if plan.AutomationsRan > 0 {
		detail, err := json.Marshal(plan)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			insert into public.automation_log (body, kind, entity_id, detail)
			values ($1, 'component', $2, $3)`,
			fmt.Sprintf("%s: %s", name, strings.Join(plan.Notes, ", ")),
			componentID, detail); err != nil {
			return nil, err
		}
	}

	return plan, nil
}

// assignBin finds a box with room and puts the quantity there, creating a box
// when nothing fits. FOR UPDATE serialises two simultaneous restocks so they
// cannot both claim the last slot in the same bin.
func assignBin(ctx context.Context, tx pgx.Tx, componentID, category string,
	qty, defaultCapacity int) (string, error) {

	// already stored somewhere? top that bin up
	var existing, existingCode string
	err := tx.QueryRow(ctx, `
		select b.id, b.code from public.box_contents k
		  join public.boxes b on b.id = k.box_id
		 where k.component_id = $1
		 order by k.qty desc limit 1`, componentID).Scan(&existing, &existingCode)
	if err == nil {
		if _, err := tx.Exec(ctx, `
			insert into public.box_contents (box_id, component_id, qty)
			values ($1, $2, $3)
			on conflict (box_id, component_id) do update set qty = public.box_contents.qty + $3`,
			existing, componentID, qty); err != nil {
			return "", err
		}
		return existingCode, nil
	}
	if !isNoRows(err) {
		return "", err
	}

	// a box with free capacity, preferring one already holding this category
	var boxID, boxCode string
	err = tx.QueryRow(ctx, `
		select b.id, b.code
		  from public.boxes b
		  left join public.box_contents k on k.box_id = b.id
		  left join public.components c   on c.id = k.component_id
		 group by b.id
		having b.capacity - coalesce(sum(k.qty), 0) >= $1
		 order by (count(*) filter (where c.category = $2)) desc,
		          b.capacity - coalesce(sum(k.qty), 0) asc
		 limit 1
		   for update of b`, qty, category).Scan(&boxID, &boxCode)

	if isNoRows(err) {
		// nothing fits: open a new box, sized to hold this and then some
		capacity := defaultCapacity
		if qty > capacity {
			capacity = int(math.Ceil(float64(qty)/100.0)) * 100
		}
		if err := tx.QueryRow(ctx, `
			insert into public.boxes (name, location, description, capacity)
			values ($1, '', $2, $3)
			returning id, code`,
			category+" Bin", "Opened automatically for "+category, capacity).
			Scan(&boxID, &boxCode); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx, `
		insert into public.box_contents (box_id, component_id, qty)
		values ($1, $2, $3)
		on conflict (box_id, component_id) do update set qty = public.box_contents.qty + $3`,
		boxID, componentID, qty); err != nil {
		return "", err
	}
	return boxCode, nil
}

// createUnits writes one row per item up to 50, and a single batch row beyond
// that — the specified threshold, and the reason a 2000-piece reel does not
// produce 2000 rows.
func createUnits(ctx context.Context, tx pgx.Tx, componentID, code string, qty int) (int, error) {
	var next int
	if err := tx.QueryRow(ctx, `
		select coalesce(max((regexp_match(unit_id, '-U(\d+)$'))[1]::int), 0)
		  from public.component_units where component_id = $1`, componentID).Scan(&next); err != nil {
		return 0, err
	}

	if qty > 50 {
		unitID := fmt.Sprintf("%s-B%03d", code, next+1)
		tag, err := tx.Exec(ctx, `
			insert into public.component_units
			  (component_id, unit_id, status, batch, batch_qty, auto)
			values ($1, $2, 'stock', true, $3, true)
			on conflict (unit_id) do nothing`, componentID, unitID, qty)
		if err != nil {
			return 0, err
		}
		return int(tag.RowsAffected()), nil
	}

	created := 0
	for i := 1; i <= qty; i++ {
		unitID := fmt.Sprintf("%s-U%03d", code, next+i)
		tag, err := tx.Exec(ctx, `
			insert into public.component_units (component_id, unit_id, status, auto)
			values ($1, $2, 'stock', true)
			on conflict (unit_id) do nothing`, componentID, unitID)
		if err != nil {
			return 0, err
		}
		created += int(tag.RowsAffected())
	}
	return created, nil
}

// -------------------------------------------------------------------- labels

func qrComponent(id, bin string) string {
	b, _ := json.Marshal(map[string]string{"type": "component", "id": id, "bin": bin})
	return string(b)
}

func QRUnit(unitID, compID string) string {
	b, _ := json.Marshal(map[string]string{"type": "unit", "unitId": unitID, "compId": compID})
	return string(b)
}

func QRBox(boxID string) string {
	b, _ := json.Marshal(map[string]string{"type": "box", "boxId": boxID})
	return string(b)
}

// QRPNG renders a QR code as a base64 data URL. The frontend can also draw from
// the qrContent string itself, which is why this is optional per label rather
// than always stored.
func QRPNG(content string, size int) (string, error) {
	if size <= 0 {
		size = 256
	}
	png, err := qrcode.Encode(content, qrcode.Medium, size)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// ------------------------------------------------------------------- helpers

// Plan for a component without writing anything — backs
// GET /api/automation/plan/:componentId, so the UI can preview.
func PlanFor(ctx context.Context, componentID string) (*Plan, error) {
	var qty int
	var unitTracked bool
	if err := db.GetDB().QueryRow(ctx,
		`select quantity, unit_tracked from public.components where id = $1`, componentID).
		Scan(&qty, &unitTracked); err != nil {
		return nil, err
	}
	auto, err := LoadAutomation(ctx, db.GetDB())
	if err != nil {
		return nil, err
	}

	p := &Plan{ReorderPoint: ReorderPoint(qty)}
	if auto.LabelsOn() {
		p.LabelsQueued = LabelCount(qty)
		p.AutomationsRan++
	}
	if auto.ReorderOn() {
		p.AutomationsRan++
	}
	if auto.UnitsOn() && unitTracked {
		if qty > 50 {
			p.UnitsCreated = 1
		} else {
			p.UnitsCreated = qty
		}
		p.AutomationsRan++
	}
	if auto.BinsOn() {
		var code string
		err := db.GetDB().QueryRow(ctx, `
			select b.code from public.box_contents k
			  join public.boxes b on b.id = k.box_id
			 where k.component_id = $1 order by k.qty desc limit 1`, componentID).Scan(&code)
		if err == nil {
			p.BinAssigned = code
			p.AutomationsRan++
		}
	}
	return p, nil
}

func nullUUID(s string) any {
	if s == "" || !models.IsUUID(s) {
		return nil
	}
	return s
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

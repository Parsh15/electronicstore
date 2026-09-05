package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
)

// Backup and restore.
//
// Both directions go through the stored functions — app_snapshot() and
// app_restore() — because the shape of a backup is a property of the schema,
// not of this Go program. If a table is added later, the function changes and
// the API keeps working.

type BackupMeta struct {
	Version    int       `json:"version"`
	TakenAt    time.Time `json:"takenAt"`
	Components int       `json:"components"`
	Projects   int       `json:"projects"`
	Funds      int       `json:"funds"`
	Boxes      int       `json:"boxes"`
	Suppliers  int       `json:"suppliers"`
}

// Create returns the whole store as one JSON document and records that it
// happened. The activity row is what /api/backup/history reads back.
func Create(ctx context.Context, actor, actorID string) ([]byte, error) {
	var snapshot []byte
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		snapshot, err = db.ScalarJSON(ctx, tx, `select public.app_snapshot()::text`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			insert into public.activity (body, glyph, color, actor, actor_id, entity_type)
			values ('Backup created', '⤓', '#8da2c8', $1, $2, 'backup')`, actor, nullUUID(actorID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

// Filename is what the browser saves the download as.
func Filename() string {
	return fmt.Sprintf("esm-backup-%s.json", time.Now().Format("2006-01-02-1504"))
}

// Restore validates the payload, then replaces the store inside a single
// transaction. app_restore() deletes and re-inserts every table, so a failure
// anywhere leaves the original data exactly as it was.
func Restore(ctx context.Context, payload []byte, actor, actorID string) (map[string]int, error) {
	if err := validateBackup(payload); err != nil {
		return nil, err
	}

	var counts map[string]int
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		var out []byte
		if err := tx.QueryRow(ctx,
			`select public.app_restore($1::jsonb, $2)::text`, payload, actor).Scan(&out); err != nil {
			return err
		}
		if err := json.Unmarshal(out, &counts); err != nil {
			return err
		}

		// Integrity check inside the same transaction: if the restore produced
		// orphans, roll the whole thing back rather than leave a broken store.
		var orphans int
		if err := tx.QueryRow(ctx, `
			select (select count(*) from public.project_parts pp
			         left join public.components c on c.id = pp.component_id
			        where c.id is null)
			     + (select count(*) from public.component_units u
			         left join public.components c on c.id = u.component_id
			        where c.id is null)
			     + (select count(*) from public.box_contents k
			         left join public.boxes b on b.id = k.box_id
			        where b.id is null)`).Scan(&orphans); err != nil {
			return err
		}
		if orphans > 0 {
			return fmt.Errorf("the backup left %d orphaned records, so nothing was changed", orphans)
		}

		_, err := tx.Exec(ctx, `
			insert into public.activity (body, glyph, color, actor, actor_id, entity_type)
			values ($1, '⟳', '#c8a06c', $2, $3, 'backup')`,
			fmt.Sprintf("Store restored from backup (%d components, %d projects)",
				counts["components"], counts["projects"]), actor, nullUUID(actorID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

// validateBackup rejects anything that is not recognisably one of our files,
// before it can touch the database.
func validateBackup(payload []byte) error {
	if len(payload) == 0 {
		return errors.New("The backup file was empty.")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return errors.New("That file is not a valid backup — it could not be read as JSON.")
	}

	// A backup must carry at least one of the collections we know how to write.
	known := []string{"components", "projects", "boxes", "funds", "suppliers", "events"}
	found := false
	for _, k := range known {
		if _, ok := probe[k]; ok {
			found = true
			break
		}
	}
	if !found {
		return errors.New("That file does not look like an Electronic Store Manager backup.")
	}

	// Version 4 is current; earlier files (and files with no version at all,
	// from the localStorage era) restore through the same function because
	// app_restore accepts both key styles.
	if raw, ok := probe["version"]; ok {
		var v int
		if err := json.Unmarshal(raw, &v); err == nil && v > 4 {
			return fmt.Errorf("that backup was made by a newer version (v%d) of the app", v)
		}
	}
	return nil
}

// History lists past backup and restore actions from the audit trail.
func History(ctx context.Context, limit int) ([]byte, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return db.QueryJSON(ctx, db.GetDB(), `
		select a.id, a.body as text, a.glyph, a.actor,
		       extract(epoch from a.created_at) * 1000 as ts
		  from public.activity a
		 where a.entity_type = 'backup'
		 order by a.created_at desc
		 limit $1`, limit)
}

// Meta summarises a snapshot without sending the whole thing — used by the
// settings screen to show what a backup would contain.
func Meta(ctx context.Context) (*BackupMeta, error) {
	m := &BackupMeta{Version: 4, TakenAt: time.Now()}
	err := db.GetDB().QueryRow(ctx, `
		select (select count(*) from public.components),
		       (select count(*) from public.projects),
		       (select count(*) from public.funds),
		       (select count(*) from public.boxes),
		       (select count(*) from public.suppliers)`).
		Scan(&m.Components, &m.Projects, &m.Funds, &m.Boxes, &m.Suppliers)
	return m, err
}

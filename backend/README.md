# Backend — Go REST API

Reads and writes PostgreSQL over a pgx pool and serves JSON to the browser.
It is the only client the database has.

## Run

```bash
cp .env.example .env     # fill in DB_* (or DATABASE_URL) and SESSION_SECRET
go mod tidy
go run .                 # :8080

go run . -create-admin   # interactive first account
go run . -migrate-only   # apply schema and exit
```

## Layout

| Path | What lives there |
| --- | --- |
| `main.go` | config, pool, migrations, the whole route table, graceful shutdown |
| `config/` | every `os.Getenv` in the program, validated once at boot |
| `db/db.go` | the pool, JSON query helpers, `InTx`, error classification |
| `db/migrate.go` | embedded SQL, applied once each, under an advisory lock |
| `middleware/` | CORS, rate limit, session auth, admin gate, recovery, logging |
| `models/` | request shapes and their validation — no SQL |
| `handlers/` | one file per resource; parse, validate, call, respond |
| `services/` | auth and sessions, automation engine, backup, reports |

## Endpoints

Auth `/api/auth` — signup, login, logout, me, refresh, change-password.

Resources, all behind a session: `/api/components`, `/api/units`,
`/api/projects`, `/api/suppliers`, `/api/boxes`, `/api/labels`, `/api/events`,
`/api/funds`, `/api/reports`, `/api/activity`, `/api/settings`, `/api/trash`,
`/api/automation`, `/api/voice/log`, `/api/search`.

Admin only: `/api/users`, `/api/backup`, `PUT /api/settings`,
`PUT /api/settings/automation`, `POST /api/automation/run`,
`DELETE /api/trash/empty`.

Open: `GET /api/health`.

## How a request is authorised

1. Read the `esm_session` cookie (HttpOnly — JavaScript cannot see it).
2. Look the token up in `sessions`, join `profiles`.
3. Reject an expired session, deleting the row on the way past.
4. Reject a deactivated account.
5. Attach the profile to the request context.
6. Admin routes re-read the role from Postgres, so a demotion applies at once.

A role in a request body or header is ignored everywhere. Sessions within a day
of expiry are extended automatically, so an active user is never signed out
mid-task.

## Transactions

Everything with more than one step runs inside `db.InTx`, which commits on
success and rolls back on any error or panic:

- restock — quantity, unit rows, labels, bin, reorder point, activity
- reserve units — availability check, unit claim, project status, activity
- complete project — deduct BOM, retire units, low-stock check, close, activity
- advance fund — status, history entry, activity
- soft delete and restore — move the row and its children, activity
- backup restore — validate, replace every table, integrity check, commit
- Smart Import — validate all rows first, then insert them all

## Automation engine

`services/automation.go`. It used to run in the browser after a save, which
meant two clients could mint the same label range or claim the same bin. Now it
runs in the same transaction as the quantity write.

Label grouping: 1–10 one each, 11–50 in fives, 51–200 in tens, 201–500 in
twenty-fives, above that in fifties. Reorder point: 20% of quantity rounded up
to the nearest five. Unit rows: individually up to 50, one batch row beyond.
Bins: top up the box that already holds the part, else the fullest box with
room, preferring one holding the same category, else open a new box.

The response carries the plan summary the UI shows as one muted line:

```json
{"labels_queued": 12, "bin_assigned": "BOX-00003", "reorder_point": 20,
 "units_created": 50, "automations_ran": 4}
```

## Reports

`services/reports.go` builds all six from the views in `database/views.sql`,
then renders the same structure as JSON, CSV (`encoding/csv`, streamed, no temp
files) or PDF (`gofpdf`, landscape A4, white background, header and footer per
page). One code path, so the three renderings cannot disagree about a number.

## Errors

`db.Classify` maps a Postgres error to a status code and a sentence written for
the person using the app. Constraint names have specific messages
("Quantity cannot go below zero"); anything unrecognised becomes a generic 500
and the detail is logged server-side only. No SQL text or stack trace ever
reaches the browser.

## Dependencies

`chi` (router), `pgx/v5` (driver and pool), `godotenv`, `golang.org/x/crypto`
(bcrypt), `golang.org/x/time` (rate limiting), `golang.org/x/term` (hidden
password prompt), `gofpdf`, `go-qrcode`. No ORM.

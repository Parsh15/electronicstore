# Electronic Store Manager

Electronics inventory management for AskSummu Pvt. Ltd., Corlim, Old Goa —
components, tracked units, storage boxes, project BOMs, labels, reports,
funding and grants, with an offline AI assistant and voice commands.

## Architecture

```
Browser  ──HTTPS──▶  Go REST API  ──pgx──▶  PostgreSQL (Supabase)
```

The browser holds no database credentials. Every read and write is an HTTP
request carrying an HttpOnly session cookie; the Go service authorises it
against a profile row loaded from the database and runs it with bound
parameters. Nothing in `frontend/` contains a key, a connection string, or SQL.

## Layout

```
electronic-store-manager/
├── frontend/          static site — deploy to Vercel
│   ├── index.html     the app, unchanged
│   ├── esm-auth.js    sign-in and user management over fetch
│   ├── esm-db.js      data layer over fetch
│   └── ai-worker.js   offline AI (Transformers.js) — untouched
├── backend/           Go REST API — deploy to Railway / Render / Fly.io
│   ├── main.go        config, pool, migrations, router, graceful shutdown
│   ├── config/        environment loading and validation
│   ├── db/            pgx pool, JSON query helpers, embedded migrations
│   ├── middleware/    CORS, rate limit, session auth, admin gate
│   ├── models/        request shapes and validation
│   ├── handlers/      one file per resource
│   └── services/      auth, automation engine, backup, reports
├── database/          the SQL, as source of truth
│   ├── schema.sql     tables, indexes, triggers
│   ├── functions.sql  stored procedures
│   ├── views.sql      report views
│   └── seed.sql       optional demo inventory
├── vercel.json
└── package.json
```

`backend/db/migrations/` holds copies of the first three SQL files, compiled
into the binary — a fresh deployment brings its own schema.

## Local development

```bash
# 1. database credentials
cp backend/.env.example backend/.env      # then fill in DB_* and SESSION_SECRET

# 2. API
cd backend && go mod tidy && go run .     # http://localhost:8080

# 3. first admin (interactive, password never echoed)
go run . -create-admin

# 4. frontend
cd ../frontend && npx serve . -l 3000     # http://localhost:3000
```

Set `ALLOWED_ORIGIN=http://localhost:3000` in `backend/.env`, and in
`frontend/index.html` set `window.ESM_API_URL = "http://localhost:8080"`.

## Deployment checklist

**1 — Database (Supabase).** Project Settings → Database → Connection string
(URI). Then either let the backend migrate on boot (`AUTO_MIGRATE=true`, the
default) or paste `database/schema.sql`, `functions.sql` and `views.sql` into
the SQL editor in that order. `seed.sql` is optional demo data.

**2 — Backend (Railway, Render or Fly.io).** Deploy `backend/`. Set:

| Variable | Value |
| --- | --- |
| `DATABASE_URL` | the Supabase URI (or the six `DB_*` variables) |
| `SESSION_SECRET` | 32+ random characters |
| `SESSION_MAX_AGE` | `604800` (7 days) |
| `ALLOWED_ORIGIN` | the exact frontend URL, e.g. `https://esm.vercel.app` |
| `ENV` | `production` |
| `PORT` | provided by the platform |

**3 — First admin.** `go run . -create-admin` against the production database,
or sign up in the app: the first account in an empty store becomes the admin.

**4 — Frontend (Vercel).** Deploy the repository root; `vercel.json` serves
`frontend/`. Before deploying, set `window.ESM_API_URL` in `index.html` to the
backend's URL.

**5 — Check.** `GET https://your-api/api/health` should return
`{"status":"ok","database":true,"schema":true}`.

Order matters: database, then backend, then the frontend that points at it.
`ALLOWED_ORIGIN` must match the frontend origin exactly — a mismatch means the
browser silently drops the session cookie and every screen looks signed out.

## What the database enforces

Not just storage. A unit's status and its faulty flag can never disagree; a
unit-tracked component is faulty when any of its units is; quantities cannot go
below zero; a component on a live BOM cannot be deleted. Codes come from
sequences, so two people adding stock at once never collide.

Every multi-step operation runs in one transaction: restock, reserve units,
complete a project, advance a fund, soft delete, restore, backup restore and
Smart Import. A failure part-way leaves nothing behind.

## Security

- bcrypt cost 12; passwords never logged, never returned, never in localStorage
- opaque session tokens in the `sessions` table, HttpOnly cookie, revocable by
  DELETE rather than by waiting for expiry
- role read from Postgres on every request — a demotion applies immediately
- parameterised queries throughout; no string-built SQL
- 10 requests/minute per IP on auth, 200 elsewhere; 10MB body cap
- CORS locked to `ALLOWED_ORIGIN`; a wildcard is rejected at startup

## Unchanged from the previous build

The UI, layout, styling, navigation, screens, workflows, labels and buttons —
the ⌘K palette, biometric unlock, voice commands, the offline AI assistant, the
health donut, the canvas hero, the constellation view, the heat calendar, and
every modal and form. Only the backend and the architecture moved.

## Known limits

- Rate limiting is per-instance and in memory; behind several replicas the
  effective limit multiplies by the replica count.
- A restore replaces the whole store. It is admin-only and transactional, but
  there is no partial merge.
- Report PDFs use the standard PDF fonts, so `₹` prints as `Rs.` — the CSV and
  the on-screen report keep the symbol.

-- ===========================================================================
-- Electronic Store Manager — schema (tables only)
-- PostgreSQL 15+ / Supabase
--
-- Run order:  schema.sql → functions.sql → views.sql → seed.sql (optional)
-- Idempotent: safe to run on a live database.
--
-- Architecture note: the Go backend connects to this database directly with a
-- privileged role and is the ONLY client. Authorization lives in Go middleware
-- (see backend/middleware/auth.go and admin.go), not in RLS policies, because
-- no untrusted client ever holds a connection. RLS is left enabled with a
-- deny-by-default posture so that an accidentally leaked anon key is useless.
-- ===========================================================================

create extension if not exists pgcrypto;

-- ---------------------------------------------------------------------------
-- Accounts
-- ---------------------------------------------------------------------------

create table if not exists public.profiles (
  id            uuid primary key default gen_random_uuid(),
  email         text not null unique,
  name          text not null default '',
  password_hash text not null default '',        -- bcrypt, cost 12, set by Go
  role          text not null default 'staff' check (role in ('admin','staff')),
  active        boolean not null default true,
  perms         jsonb,
  created_by    text default '',
  last_login_at timestamptz,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now()
);

create index if not exists profiles_email_idx on public.profiles (lower(email));

-- Server-side sessions. The browser only ever holds an opaque token in an
-- HttpOnly cookie; everything about the session lives here.
create table if not exists public.sessions (
  id         uuid primary key default gen_random_uuid(),
  user_id    uuid not null references public.profiles(id) on delete cascade,
  token      text not null unique,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  user_agent text,
  ip_address text
);

create index if not exists sessions_user_idx    on public.sessions (user_id);
create index if not exists sessions_expires_idx on public.sessions (expires_at);

-- ---------------------------------------------------------------------------
-- Store settings (single row) + automation audit
-- ---------------------------------------------------------------------------

create table if not exists public.settings (
  id                smallint primary key default 1 check (id = 1),
  currency_symbol   text    not null default '₹',
  low_stock_default integer not null default 10,
  comp_prefix       text    not null default 'CMP',
  box_prefix        text    not null default 'BOX',
  date_fmt          text    not null default 'medium' check (date_fmt in ('short','medium','long')),
  automation        jsonb   not null default
    '{"labels":true,"bins":true,"reorder":true,"units":true,"boxCapacity":250}'::jsonb,
  updated_at        timestamptz not null default now()
);

insert into public.settings (id) values (1) on conflict (id) do nothing;

create table if not exists public.automation_log (
  id         uuid primary key default gen_random_uuid(),
  body       text not null,
  kind       text default 'auto',
  entity_id  text default '',
  detail     jsonb,
  created_at timestamptz not null default now()
);

create index if not exists automation_log_created_idx on public.automation_log (created_at desc);

-- ---------------------------------------------------------------------------
-- Code sequences — the database assigns codes, so two devices never collide
-- ---------------------------------------------------------------------------

create sequence if not exists public.component_code_seq;
create sequence if not exists public.box_code_seq;
create sequence if not exists public.fund_code_seq;
create sequence if not exists public.project_code_seq;

create or replace function public.next_code(prefix text, seq regclass)
returns text language sql volatile as $$
  select prefix || '-' || lpad(nextval(seq)::text, 5, '0');
$$;

-- ---------------------------------------------------------------------------
-- Suppliers
-- ---------------------------------------------------------------------------

create table if not exists public.suppliers (
  id         uuid primary key default gen_random_uuid(),
  name       text not null,
  contact    text default '',
  email      text default '',
  phone      text default '',
  website    text default '',
  notes      text default '',
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create unique index if not exists suppliers_name_key on public.suppliers (lower(name));

-- ---------------------------------------------------------------------------
-- Projects (before components: units reference projects)
-- ---------------------------------------------------------------------------

create table if not exists public.projects (
  id           uuid primary key default gen_random_uuid(),
  code         text unique not null default public.next_code('PRJ','public.project_code_seq'),
  name         text not null,
  description  text default '',
  detail       text default '',
  file_name    text default '',
  file_url     text,
  status       text not null default 'planned'
               check (status in ('planned','active','on-hold','complete','cancelled')),
  started_at   timestamptz,
  completed_at timestamptz,
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now()
);

-- ---------------------------------------------------------------------------
-- Boxes (before components: components may name a home box)
-- ---------------------------------------------------------------------------

create table if not exists public.boxes (
  id          uuid primary key default gen_random_uuid(),
  code        text unique not null default public.next_code('BOX','public.box_code_seq'),
  name        text not null,
  location    text default '',
  description text default '',
  capacity    integer not null default 250 check (capacity > 0),
  image_name  text default '',
  image_url   text,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now()
);

-- ---------------------------------------------------------------------------
-- Components and tracked units
-- ---------------------------------------------------------------------------

create table if not exists public.components (
  id            uuid primary key default gen_random_uuid(),
  code          text unique not null default public.next_code('CMP','public.component_code_seq'),
  name          text not null,
  category      text default 'Uncategorised',
  location      text default '',
  quantity      integer not null default 0 check (quantity >= 0),
  min_stock     integer,
  reorder_point integer,
  price         numeric(12,2),
  supplier_id   uuid references public.suppliers on delete set null,
  supplier_name text default '',
  faulty        boolean not null default false,
  unit_tracked  boolean not null default false,
  image_name    text default '',
  image_url     text,
  expiry        date,
  datasheet     text default '',
  notes         text default '',
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now()
);

create index if not exists components_category_idx on public.components (category);
create index if not exists components_supplier_idx on public.components (supplier_id);
create index if not exists components_name_idx     on public.components (lower(name));

create table if not exists public.component_units (
  id           uuid primary key default gen_random_uuid(),
  component_id uuid not null references public.components on delete cascade,
  unit_id      text not null unique,
  status       text not null default 'stock'
               check (status in ('stock','reserved','in-use','faulty','retired')),
  faulty       boolean not null default false,
  project_id   uuid references public.projects on delete set null,
  batch        boolean not null default false,
  batch_qty    integer,
  auto         boolean not null default false,
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now()
);

create index if not exists component_units_component_idx on public.component_units (component_id);
create index if not exists component_units_project_idx   on public.component_units (project_id);
create index if not exists component_units_status_idx    on public.component_units (status);

create table if not exists public.box_contents (
  box_id       uuid not null references public.boxes on delete cascade,
  component_id uuid not null references public.components on delete cascade,
  qty          integer not null default 1 check (qty > 0),
  primary key (box_id, component_id)
);

-- ---------------------------------------------------------------------------
-- Bills of materials
-- ---------------------------------------------------------------------------

create table if not exists public.project_parts (
  id           uuid primary key default gen_random_uuid(),
  project_id   uuid not null references public.projects on delete cascade,
  component_id uuid not null references public.components on delete restrict,
  qty          integer not null default 1 check (qty > 0),
  status       text not null default 'planned'
               check (status in ('planned','reserved','taken','returned')),
  unit_ids     text[] not null default '{}',
  taken_at     timestamptz,
  taken_by     text default '',
  unique (project_id, component_id)
);

-- ---------------------------------------------------------------------------
-- Events and funding
-- ---------------------------------------------------------------------------

create table if not exists public.events (
  id         uuid primary key default gen_random_uuid(),
  name       text not null,
  org        text default '',
  type       text not null default 'Competition'
             check (type in ('Competition','Grant Cycle','Exhibition','Hackathon','Conference','Other')),
  event_date date,
  venue      text default '',
  notes      text default '',
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table if not exists public.funds (
  id         uuid primary key default gen_random_uuid(),
  code       text unique not null default public.next_code('FND','public.fund_code_seq'),
  name       text not null,
  provider   text default '',
  kind       text not null default 'Grant'
             check (kind in ('Grant','Competition Prize','Sponsorship','Internal Budget','Loan','Other')),
  currency   char(3) not null default 'INR' check (currency in ('INR','USD')),
  event_id   uuid references public.events on delete set null,
  requested  numeric(14,2),
  approved   numeric(14,2),
  received   numeric(14,2),
  applied_on date,
  deadline   date,
  status     text not null default 'Draft'
             check (status in ('Draft','Applied','Under Review','Approved','Received','Rejected','Closed')),
  contact    text default '',
  ref        text default '',
  notes      text default '',
  docs       text default '',
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create index if not exists funds_status_idx   on public.funds (status);
create index if not exists funds_deadline_idx on public.funds (deadline);

create table if not exists public.fund_projects (
  fund_id    uuid not null references public.funds on delete cascade,
  project_id uuid not null references public.projects on delete cascade,
  primary key (fund_id, project_id)
);

create table if not exists public.fund_parts (
  id           uuid primary key default gen_random_uuid(),
  fund_id      uuid not null references public.funds on delete cascade,
  component_id uuid not null references public.components on delete restrict,
  qty          integer not null default 1 check (qty > 0),
  unique (fund_id, component_id)
);

create table if not exists public.fund_history (
  id         uuid primary key default gen_random_uuid(),
  fund_id    uuid not null references public.funds on delete cascade,
  status     text not null,
  note       text default '',
  created_by text default '',
  created_at timestamptz not null default now()
);

-- ---------------------------------------------------------------------------
-- Comments — one row, exactly one owner column set
-- ---------------------------------------------------------------------------

create table if not exists public.comments (
  id           uuid primary key default gen_random_uuid(),
  component_id uuid references public.components on delete cascade,
  unit_id      text references public.component_units (unit_id) on delete cascade,
  box_id       uuid references public.boxes on delete cascade,
  project_id   uuid references public.projects on delete cascade,
  fund_id      uuid references public.funds on delete cascade,
  author       text not null default '',
  body         text not null,
  tag          text not null default 'General'
               check (tag in ('General','Faulty Note','Restock Note','Build Note','Funding Note')),
  created_at   timestamptz not null default now(),
  constraint comments_one_owner check (
    (component_id is not null)::int + (unit_id is not null)::int + (box_id is not null)::int
    + (project_id is not null)::int + (fund_id is not null)::int = 1
  )
);

create index if not exists comments_component_idx on public.comments (component_id);
create index if not exists comments_project_idx   on public.comments (project_id);

-- ---------------------------------------------------------------------------
-- Labels
-- ---------------------------------------------------------------------------

create table if not exists public.labels (
  id           uuid primary key default gen_random_uuid(),
  label_id     text not null unique,
  type         text not null check (type in ('component','unit','box','batch')),
  component_id uuid references public.components on delete cascade,
  unit_id      uuid references public.component_units on delete cascade,
  box_id       uuid references public.boxes on delete cascade,
  data         jsonb,
  printed      boolean not null default false,
  created_at   timestamptz not null default now(),
  created_by   uuid references public.profiles(id) on delete set null
);

create index if not exists labels_component_idx on public.labels (component_id);
create index if not exists labels_unit_idx      on public.labels (unit_id);
create index if not exists labels_box_idx       on public.labels (box_id);
create index if not exists labels_type_idx      on public.labels (type);

-- ---------------------------------------------------------------------------
-- Saved reports
-- ---------------------------------------------------------------------------

create table if not exists public.reports (
  id           uuid primary key default gen_random_uuid(),
  user_id      uuid references public.profiles(id) on delete set null,
  type         text not null,
  name         text not null,
  data         jsonb,
  generated_at timestamptz not null default now()
);

create index if not exists reports_type_idx on public.reports (type, generated_at desc);

-- ---------------------------------------------------------------------------
-- Voice command log (analytics only — recognition stays in the browser)
-- ---------------------------------------------------------------------------

create table if not exists public.voice_log (
  id         uuid primary key default gen_random_uuid(),
  user_id    uuid references public.profiles(id) on delete set null,
  command    text not null,
  action     text,
  success    boolean,
  created_at timestamptz not null default now()
);

create index if not exists voice_log_created_idx on public.voice_log (created_at desc);

-- ---------------------------------------------------------------------------
-- Activity log and trash
-- ---------------------------------------------------------------------------

create table if not exists public.activity (
  id          uuid primary key default gen_random_uuid(),
  body        text not null,
  glyph       text default '•',
  color       text default '#8da2c8',
  actor       text default '',
  actor_id    uuid references public.profiles(id) on delete set null,
  entity_type text default '',
  entity_id   text default '',
  created_at  timestamptz not null default now()
);

create index if not exists activity_created_idx on public.activity (created_at desc);
create index if not exists activity_entity_idx  on public.activity (entity_type, entity_id);

create table if not exists public.trash (
  tid        uuid primary key default gen_random_uuid(),
  kind       text not null,
  label      text default '',
  payload    jsonb not null,
  deleted_by text default '',
  deleted_at timestamptz not null default now()
);

create index if not exists trash_deleted_idx on public.trash (deleted_at desc);

-- ---------------------------------------------------------------------------
-- Migration bookkeeping (written by backend/db/migrate.go)
-- ---------------------------------------------------------------------------

create table if not exists public.schema_migrations (
  version    text primary key,
  applied_at timestamptz not null default now()
);

-- ---------------------------------------------------------------------------
-- updated_at triggers
-- ---------------------------------------------------------------------------

create or replace function public.touch_updated_at()
returns trigger language plpgsql as $$
begin new.updated_at = now(); return new; end;
$$;

do $$
declare t text;
begin
  foreach t in array array['profiles','settings','suppliers','projects','components',
                           'component_units','boxes','events','funds']
  loop
    execute format('drop trigger if exists %I_touch on public.%I', t, t);
    execute format('create trigger %I_touch before update on public.%I
                    for each row execute function public.touch_updated_at()', t, t);
  end loop;
end $$;

-- ---------------------------------------------------------------------------
-- Integrity triggers
-- ---------------------------------------------------------------------------

-- status and faulty can never disagree; a stock or faulty unit holds no project
create or replace function public.normalise_unit()
returns trigger language plpgsql as $$
begin
  if new.faulty then
    new.status := 'faulty'; new.project_id := null;
  elsif new.status = 'faulty' then
    new.faulty := true; new.project_id := null;
  elsif new.status in ('in-use','reserved') then
    if new.project_id is null then new.status := 'stock'; end if;
  elsif new.project_id is not null then
    new.status := 'in-use';
  end if;
  return new;
end $$;

drop trigger if exists component_units_normalise on public.component_units;
create trigger component_units_normalise before insert or update on public.component_units
  for each row execute function public.normalise_unit();

-- a unit-tracked component is faulty when any of its units is
create or replace function public.sync_component_faulty()
returns trigger language plpgsql as $$
declare cid uuid;
begin
  cid := coalesce(new.component_id, old.component_id);
  update public.components c
     set faulty = exists (select 1 from public.component_units u
                          where u.component_id = cid and u.faulty)
   where c.id = cid and c.unit_tracked;
  return null;
end $$;

drop trigger if exists component_units_faulty on public.component_units;
create trigger component_units_faulty after insert or update or delete on public.component_units
  for each row execute function public.sync_component_faulty();

-- ---------------------------------------------------------------------------
-- RLS: deny by default. The Go backend uses a privileged role and bypasses
-- these; anon/authenticated clients get nothing, by design.
-- ---------------------------------------------------------------------------

do $$
declare t text;
begin
  foreach t in array array['profiles','sessions','settings','automation_log','suppliers',
                           'projects','components','component_units','boxes','box_contents',
                           'project_parts','events','funds','fund_projects','fund_parts',
                           'fund_history','comments','labels','reports','voice_log',
                           'activity','trash','schema_migrations']
  loop
    execute format('alter table public.%I enable row level security', t);
    execute format('drop policy if exists "member read" on public.%I', t);
    execute format('drop policy if exists "member insert" on public.%I', t);
    execute format('drop policy if exists "member update" on public.%I', t);
    execute format('drop policy if exists "admin delete" on public.%I', t);
    execute format('drop policy if exists "admin write" on public.%I', t);
  end loop;
end $$;

-- Revoke the PostgREST roles entirely: nothing reaches this database except Go.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    execute 'revoke all on all tables in schema public from anon';
    execute 'revoke all on all functions in schema public from anon';
  end if;
  if exists (select 1 from pg_roles where rolname = 'authenticated') then
    execute 'revoke all on all tables in schema public from authenticated';
    execute 'revoke all on all functions in schema public from authenticated';
  end if;
end $$;

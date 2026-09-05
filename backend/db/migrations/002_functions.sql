-- ===========================================================================
-- Electronic Store Manager — stored functions
-- Run after schema.sql. Idempotent.
--
-- Every function here is a single atomic unit. The Go backend calls them with
--   SELECT public.restock_component($1, $2, $3, $4)
-- so a multi-step inventory operation is one round trip and one transaction.
-- Where Go needs to interleave extra work (label generation, bin assignment),
-- it opens its own transaction and calls these inside it — nesting is safe
-- because these functions contain no explicit COMMIT.
--
-- Actor: Go passes the acting user's display name explicitly. There is no
-- auth.uid() here; identity is established in the Go middleware.
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- restock_component(component, add, note, actor)
--   → adds stock, generates unit IDs when tracked, attaches the note,
--     logs activity. Returns the updated component row.
-- ---------------------------------------------------------------------------
create or replace function public.restock_component(
  p_component_id uuid,
  p_add          integer,
  p_note         text default null,
  p_actor        text default 'system')
returns public.components language plpgsql as $$
declare c public.components; i integer; nextn integer;
begin
  if p_add is null or p_add = 0 then raise exception 'restock quantity must be non-zero'; end if;

  update public.components set quantity = greatest(0, quantity + p_add)
   where id = p_component_id returning * into c;
  if c.id is null then raise exception 'component % not found', p_component_id; end if;

  if c.unit_tracked and p_add > 0 then
    select coalesce(max((regexp_match(unit_id, '-U(\d+)$'))[1]::int), 0)
      into nextn from public.component_units where component_id = c.id;
    for i in 1..p_add loop
      insert into public.component_units (component_id, unit_id, status, auto)
      values (c.id, c.code || '-U' || lpad((nextn + i)::text, 3, '0'), 'stock', true)
      on conflict (unit_id) do nothing;
    end loop;
  end if;

  if p_note is not null and length(trim(p_note)) > 0 then
    insert into public.comments (component_id, author, body, tag)
    values (c.id, p_actor, trim(p_note), 'Restock Note');
  end if;

  insert into public.activity (body, glyph, color, actor, entity_type, entity_id)
  values (case when p_add > 0 then 'Restocked "' else 'Deducted from "' end || c.name || '" '
            || case when p_add > 0 then '+' else '' end || p_add,
          case when p_add > 0 then '↑' else '↓' end,
          case when p_add > 0 then '#5faa87' else '#c8a06c' end,
          p_actor, 'component', c.id::text);
  return c;
end $$;

-- ---------------------------------------------------------------------------
-- set_unit_status(unit_id, status, project, actor)
--   → moves one unit between stock / reserved / in-use / faulty / retired.
-- ---------------------------------------------------------------------------
create or replace function public.set_unit_status(
  p_unit_id    text,
  p_status     text,
  p_project_id uuid default null,
  p_actor      text default 'system')
returns public.component_units language plpgsql as $$
declare u public.component_units;
begin
  if p_status not in ('stock','reserved','in-use','faulty','retired') then
    raise exception 'invalid unit status %', p_status;
  end if;

  update public.component_units
     set status     = p_status,
         faulty     = (p_status = 'faulty'),
         project_id = case when p_status in ('reserved','in-use') then p_project_id else null end
   where unit_id = p_unit_id
  returning * into u;
  if u.id is null then raise exception 'unit % not found', p_unit_id; end if;

  insert into public.activity (body, glyph, color, actor, entity_type, entity_id)
  values ('Unit ' || p_unit_id || ' → ' || p_status,
          case p_status when 'faulty' then '!' when 'stock' then '↩' else '→' end,
          case p_status when 'faulty' then '#c0655f' else '#8da2c8' end,
          p_actor, 'unit', u.id::text);
  return u;
end $$;

-- ---------------------------------------------------------------------------
-- reserve_project_units(project, actor)
--   → claims free units for every BOM line, marks the project active.
--     Refuses (rolls back) if any line cannot be fully covered.
-- ---------------------------------------------------------------------------
create or replace function public.reserve_project_units(
  p_project_id uuid,
  p_actor      text default 'system')
returns integer language plpgsql as $$
declare pp record; got integer; total integer := 0; cname text;
begin
  if not exists (select 1 from public.projects where id = p_project_id) then
    raise exception 'project % not found', p_project_id;
  end if;

  for pp in select * from public.project_parts where project_id = p_project_id loop
    select name into cname from public.components where id = pp.component_id;

    with pick as (
      select id from public.component_units
       where component_id = pp.component_id and status = 'stock' and not faulty
       order by created_at
       limit pp.qty
       for update skip locked
    ), moved as (
      update public.component_units u
         set status = 'reserved', project_id = p_project_id
        from pick where u.id = pick.id
      returning u.unit_id
    )
    select count(*), coalesce(array_agg(unit_id), '{}') into got, pp.unit_ids from moved;

    -- untracked components are covered by bulk quantity instead of unit rows
    if got = 0 and not (select unit_tracked from public.components where id = pp.component_id) then
      if (select quantity from public.components where id = pp.component_id) < pp.qty then
        raise exception 'not enough "%" in stock (need %)', cname, pp.qty;
      end if;
    elsif got < pp.qty then
      raise exception 'only % of % units of "%" available', got, pp.qty, cname;
    end if;

    update public.project_parts
       set status = 'reserved', unit_ids = pp.unit_ids
     where id = pp.id;
    total := total + got;
  end loop;

  update public.projects
     set status = 'active', started_at = coalesce(started_at, now())
   where id = p_project_id;

  insert into public.activity (body, glyph, color, actor, entity_type, entity_id)
  select 'Reserved parts for "' || name || '"', '⊙', '#8da2c8', p_actor, 'project', id::text
    from public.projects where id = p_project_id;
  return total;
end $$;

-- ---------------------------------------------------------------------------
-- complete_project(project, actor)
--   → deducts every BOM quantity from stock, retires the reserved units,
--     closes the project, flags anything that fell to or below min stock.
-- ---------------------------------------------------------------------------
create or replace function public.complete_project(
  p_project_id uuid,
  p_actor      text default 'system')
returns jsonb language plpgsql as $$
declare low jsonb; pname text;
begin
  select name into pname from public.projects where id = p_project_id;
  if pname is null then raise exception 'project % not found', p_project_id; end if;

  update public.components c
     set quantity = greatest(0, c.quantity - pp.qty)
    from public.project_parts pp
   where pp.project_id = p_project_id and pp.component_id = c.id;

  update public.project_parts
     set status = 'taken', taken_at = now(), taken_by = p_actor
   where project_id = p_project_id;

  update public.component_units
     set status = 'retired', project_id = null
   where project_id = p_project_id;

  update public.projects
     set status = 'complete', completed_at = now()
   where id = p_project_id;

  -- which components dropped to their reorder threshold as a result
  select coalesce(jsonb_agg(jsonb_build_object(
           'id', c.id, 'code', c.code, 'name', c.name,
           'quantity', c.quantity, 'minStock', c.min_stock)), '[]'::jsonb)
    into low
    from public.components c
    join public.project_parts pp on pp.component_id = c.id
   where pp.project_id = p_project_id
     and c.min_stock is not null
     and c.quantity <= c.min_stock;

  insert into public.activity (body, glyph, color, actor, entity_type, entity_id)
  values ('Completed project "' || pname || '"', '✓', '#5faa87', p_actor, 'project', p_project_id::text);

  if jsonb_array_length(low) > 0 then
    insert into public.automation_log (body, kind, entity_id, detail)
    values (jsonb_array_length(low) || ' component(s) hit low stock after completing "' || pname || '"',
            'low-stock', p_project_id::text, low);
  end if;

  return jsonb_build_object('project', pname, 'lowStock', low);
end $$;

-- ---------------------------------------------------------------------------
-- advance_fund(fund, status, note, actor)
--   → status change plus its audit line, atomically.
-- ---------------------------------------------------------------------------
create or replace function public.advance_fund(
  p_fund_id uuid,
  p_status  text,
  p_note    text default '',
  p_actor   text default 'system')
returns public.funds language plpgsql as $$
declare f public.funds;
begin
  update public.funds set status = p_status where id = p_fund_id returning * into f;
  if f.id is null then raise exception 'fund % not found', p_fund_id; end if;

  insert into public.fund_history (fund_id, status, note, created_by)
  values (p_fund_id, p_status, coalesce(p_note, ''), p_actor);

  insert into public.activity (body, glyph, color, actor, entity_type, entity_id)
  values ('Fund "' || f.name || '" → ' || p_status, '₹', '#c8a06c', p_actor, 'fund', f.id::text);
  return f;
end $$;

-- ---------------------------------------------------------------------------
-- soft_delete(kind, id, label, actor)  /  restore_trash(tid, actor)
--   Generic trash bin. `kind` is the table name; the row is serialised whole
--   into trash.payload so a restore is lossless.
-- ---------------------------------------------------------------------------
create or replace function public.soft_delete(
  p_kind  text,
  p_id    uuid,
  p_label text default '',
  p_actor text default 'system')
returns uuid language plpgsql as $$
declare payload jsonb; new_tid uuid;
begin
  if p_kind not in ('components','projects','boxes','funds','events','suppliers','labels') then
    raise exception 'cannot soft delete %', p_kind;
  end if;

  execute format('select to_jsonb(t) from public.%I t where t.id = $1', p_kind)
     into payload using p_id;
  if payload is null then raise exception '% % not found', p_kind, p_id; end if;

  -- children travel with the parent so a restore brings the whole thing back
  if p_kind = 'components' then
    payload := payload || jsonb_build_object(
      '_units',    coalesce((select jsonb_agg(to_jsonb(u)) from public.component_units u where u.component_id = p_id), '[]'::jsonb),
      '_comments', coalesce((select jsonb_agg(to_jsonb(m)) from public.comments m where m.component_id = p_id), '[]'::jsonb));
  elsif p_kind = 'projects' then
    payload := payload || jsonb_build_object(
      '_parts',    coalesce((select jsonb_agg(to_jsonb(pp)) from public.project_parts pp where pp.project_id = p_id), '[]'::jsonb),
      '_comments', coalesce((select jsonb_agg(to_jsonb(m)) from public.comments m where m.project_id = p_id), '[]'::jsonb));
  elsif p_kind = 'boxes' then
    payload := payload || jsonb_build_object(
      '_contents', coalesce((select jsonb_agg(to_jsonb(k)) from public.box_contents k where k.box_id = p_id), '[]'::jsonb),
      '_comments', coalesce((select jsonb_agg(to_jsonb(m)) from public.comments m where m.box_id = p_id), '[]'::jsonb));
  elsif p_kind = 'funds' then
    payload := payload || jsonb_build_object(
      '_projects', coalesce((select jsonb_agg(to_jsonb(fp)) from public.fund_projects fp where fp.fund_id = p_id), '[]'::jsonb),
      '_parts',    coalesce((select jsonb_agg(to_jsonb(fa)) from public.fund_parts fa where fa.fund_id = p_id), '[]'::jsonb),
      '_history',  coalesce((select jsonb_agg(to_jsonb(h)) from public.fund_history h where h.fund_id = p_id), '[]'::jsonb));
  end if;

  insert into public.trash (kind, label, payload, deleted_by)
  values (p_kind, p_label, payload, p_actor)
  returning tid into new_tid;

  execute format('delete from public.%I where id = $1', p_kind) using p_id;

  insert into public.activity (body, glyph, color, actor, entity_type, entity_id)
  values ('Deleted ' || rtrim(p_kind, 's') || ' "' || coalesce(nullif(p_label, ''), p_id::text) || '"',
          '×', '#c0655f', p_actor, p_kind, p_id::text);
  return new_tid;
end $$;

create or replace function public.restore_trash(
  p_tid   uuid,
  p_actor text default 'system')
returns uuid language plpgsql as $$
declare rec public.trash; base jsonb; child jsonb; rid uuid;
begin
  select * into rec from public.trash where tid = p_tid;
  if rec.tid is null then raise exception 'trash entry % not found', p_tid; end if;

  base := rec.payload - '_units' - '_comments' - '_parts' - '_contents' - '_projects' - '_history';
  execute format('insert into public.%I select * from jsonb_populate_record(null::public.%I, $1)',
                 rec.kind, rec.kind) using base;
  rid := (base->>'id')::uuid;

  if rec.kind = 'components' then
    for child in select * from jsonb_array_elements(coalesce(rec.payload->'_units', '[]'::jsonb)) loop
      insert into public.component_units select * from jsonb_populate_record(null::public.component_units, child)
      on conflict (unit_id) do nothing;
    end loop;
  elsif rec.kind = 'projects' then
    for child in select * from jsonb_array_elements(coalesce(rec.payload->'_parts', '[]'::jsonb)) loop
      insert into public.project_parts select * from jsonb_populate_record(null::public.project_parts, child)
      on conflict do nothing;
    end loop;
  elsif rec.kind = 'boxes' then
    for child in select * from jsonb_array_elements(coalesce(rec.payload->'_contents', '[]'::jsonb)) loop
      insert into public.box_contents select * from jsonb_populate_record(null::public.box_contents, child)
      on conflict do nothing;
    end loop;
  elsif rec.kind = 'funds' then
    for child in select * from jsonb_array_elements(coalesce(rec.payload->'_projects', '[]'::jsonb)) loop
      insert into public.fund_projects select * from jsonb_populate_record(null::public.fund_projects, child)
      on conflict do nothing;
    end loop;
    for child in select * from jsonb_array_elements(coalesce(rec.payload->'_parts', '[]'::jsonb)) loop
      insert into public.fund_parts select * from jsonb_populate_record(null::public.fund_parts, child)
      on conflict do nothing;
    end loop;
    for child in select * from jsonb_array_elements(coalesce(rec.payload->'_history', '[]'::jsonb)) loop
      insert into public.fund_history select * from jsonb_populate_record(null::public.fund_history, child);
    end loop;
  end if;

  for child in select * from jsonb_array_elements(coalesce(rec.payload->'_comments', '[]'::jsonb)) loop
    insert into public.comments select * from jsonb_populate_record(null::public.comments, child);
  end loop;

  delete from public.trash where tid = p_tid;

  insert into public.activity (body, glyph, color, actor, entity_type, entity_id)
  values ('Restored ' || rtrim(rec.kind, 's') || ' "' || coalesce(nullif(rec.label, ''), rid::text) || '"',
          '↩', '#5faa87', p_actor, rec.kind, rid::text);
  return rid;
end $$;

-- ---------------------------------------------------------------------------
-- app_snapshot()
--   → the entire store as one JSON document. Used by /api/backup/create and
--     by the frontend's first full load.
-- ---------------------------------------------------------------------------
create or replace function public.app_snapshot()
returns jsonb language sql stable as $$
  select jsonb_build_object(
    'version', 4,
    'takenAt', now(),
    'settings', (select to_jsonb(s) from public.settings s where id = 1),
    'suppliers', coalesce((select jsonb_agg(to_jsonb(x) order by x.name) from public.suppliers x), '[]'::jsonb),
    'components', coalesce((select jsonb_agg(
        to_jsonb(c) || jsonb_build_object(
          'units', coalesce((select jsonb_agg(to_jsonb(u) order by u.unit_id)
                             from public.component_units u where u.component_id = c.id), '[]'::jsonb),
          'comments', coalesce((select jsonb_agg(to_jsonb(m) order by m.created_at desc)
                             from public.comments m where m.component_id = c.id), '[]'::jsonb))
        order by c.code) from public.components c), '[]'::jsonb),
    'boxes', coalesce((select jsonb_agg(
        to_jsonb(b) || jsonb_build_object(
          'contents', coalesce((select jsonb_agg(to_jsonb(k)) from public.box_contents k where k.box_id = b.id), '[]'::jsonb),
          'comments', coalesce((select jsonb_agg(to_jsonb(m) order by m.created_at desc)
                             from public.comments m where m.box_id = b.id), '[]'::jsonb))
        order by b.code) from public.boxes b), '[]'::jsonb),
    'projects', coalesce((select jsonb_agg(
        to_jsonb(p) || jsonb_build_object(
          'parts', coalesce((select jsonb_agg(to_jsonb(pp)) from public.project_parts pp where pp.project_id = p.id), '[]'::jsonb),
          'comments', coalesce((select jsonb_agg(to_jsonb(m) order by m.created_at desc)
                             from public.comments m where m.project_id = p.id), '[]'::jsonb))
        order by p.created_at desc) from public.projects p), '[]'::jsonb),
    'funds', coalesce((select jsonb_agg(
        to_jsonb(f) || jsonb_build_object(
          'projectIds', coalesce((select jsonb_agg(fp.project_id) from public.fund_projects fp where fp.fund_id = f.id), '[]'::jsonb),
          'parts', coalesce((select jsonb_agg(to_jsonb(fa)) from public.fund_parts fa where fa.fund_id = f.id), '[]'::jsonb),
          'history', coalesce((select jsonb_agg(to_jsonb(h) order by h.created_at) from public.fund_history h where h.fund_id = f.id), '[]'::jsonb))
        order by f.created_at desc) from public.funds f), '[]'::jsonb),
    'events', coalesce((select jsonb_agg(to_jsonb(e) order by e.event_date) from public.events e), '[]'::jsonb),
    'labels', coalesce((select jsonb_agg(to_jsonb(l) order by l.created_at desc) from public.labels l), '[]'::jsonb),
    'activity', coalesce((select jsonb_agg(to_jsonb(a)) from
        (select * from public.activity order by created_at desc limit 200) a), '[]'::jsonb),
    'trash', coalesce((select jsonb_agg(to_jsonb(t) order by t.deleted_at desc) from public.trash t), '[]'::jsonb),
    'automationLog', coalesce((select jsonb_agg(to_jsonb(g)) from
        (select * from public.automation_log order by created_at desc limit 100) g), '[]'::jsonb)
  );
$$;

-- ---------------------------------------------------------------------------
-- app_restore(data, actor)
--   → replaces the whole store. One statement, therefore one transaction:
--     any failure leaves the database untouched. Accepts both the snake_case
--     shape produced by app_snapshot() and the camelCase shape the browser
--     kept in localStorage, so old backup files still restore.
-- ---------------------------------------------------------------------------
create or replace function public.app_restore(
  p_data  jsonb,
  p_actor text default 'system')
returns jsonb language plpgsql as $$
declare
  n_comp int := 0; n_proj int := 0; n_box int := 0; n_fund int := 0;
  n_sup int := 0; n_evt int := 0;
  r jsonb; sub jsonb; new_id uuid;
begin
  if p_data is null or jsonb_typeof(p_data) <> 'object' then
    raise exception 'backup payload must be a JSON object';
  end if;

  delete from public.comments;
  delete from public.project_parts;
  delete from public.box_contents;
  delete from public.fund_parts;
  delete from public.fund_projects;
  delete from public.fund_history;
  delete from public.labels;
  delete from public.component_units;
  delete from public.funds;
  delete from public.events;
  delete from public.projects;
  delete from public.components;
  delete from public.boxes;
  delete from public.suppliers;

  for r in select * from jsonb_array_elements(coalesce(p_data->'suppliers', '[]'::jsonb)) loop
    insert into public.suppliers (id, name, contact, email, phone, website, notes)
    values (coalesce(nullif(r->>'id','')::uuid, gen_random_uuid()), r->>'name',
            coalesce(r->>'contact',''), coalesce(r->>'email',''), coalesce(r->>'phone',''),
            coalesce(r->>'website',''), coalesce(r->>'notes',''))
    on conflict do nothing;
    n_sup := n_sup + 1;
  end loop;

  for r in select * from jsonb_array_elements(coalesce(p_data->'events', '[]'::jsonb)) loop
    insert into public.events (id, name, org, type, event_date, venue, notes)
    values (coalesce(nullif(r->>'id','')::uuid, gen_random_uuid()), r->>'name',
            coalesce(r->>'org',''), coalesce(nullif(r->>'type',''),'Competition'),
            nullif(coalesce(r->>'event_date', r->>'date'),'')::date,
            coalesce(r->>'venue',''), coalesce(r->>'notes',''));
    n_evt := n_evt + 1;
  end loop;

  for r in select * from jsonb_array_elements(coalesce(p_data->'boxes', '[]'::jsonb)) loop
    insert into public.boxes (id, code, name, location, description, capacity, image_name)
    values (coalesce(nullif(r->>'id','')::uuid, gen_random_uuid()),
            coalesce(nullif(r->>'code',''), public.next_code('BOX','public.box_code_seq')),
            r->>'name', coalesce(r->>'location',''), coalesce(r->>'description',''),
            coalesce(nullif(r->>'capacity','')::int, 250),
            coalesce(r->>'image_name', r->>'imageName',''));
    n_box := n_box + 1;
  end loop;

  for r in select * from jsonb_array_elements(coalesce(p_data->'components', '[]'::jsonb)) loop
    new_id := coalesce(nullif(r->>'id','')::uuid, gen_random_uuid());
    insert into public.components (id, code, name, category, location, quantity, min_stock,
                                   reorder_point, price, supplier_name, faulty, unit_tracked,
                                   image_name, expiry, datasheet, notes)
    values (new_id,
            coalesce(nullif(r->>'code',''), public.next_code('CMP','public.component_code_seq')),
            r->>'name', coalesce(nullif(r->>'category',''),'Uncategorised'),
            coalesce(r->>'location',''), coalesce(nullif(r->>'quantity','')::int, 0),
            nullif(coalesce(r->>'min_stock', r->>'minStock'),'')::int,
            nullif(coalesce(r->>'reorder_point', r->>'reorderPoint'),'')::int,
            nullif(r->>'price','')::numeric,
            coalesce(r->>'supplier_name', r->>'supplier',''),
            coalesce(nullif(r->>'faulty','')::boolean, false),
            coalesce(nullif(coalesce(r->>'unit_tracked', r->>'unitTracked'),'')::boolean, false),
            coalesce(r->>'image_name', r->>'imageName',''),
            nullif(r->>'expiry','')::date, coalesce(r->>'datasheet',''), coalesce(r->>'notes',''));
    n_comp := n_comp + 1;

    for sub in select * from jsonb_array_elements(coalesce(r->'units', '[]'::jsonb)) loop
      insert into public.component_units (component_id, unit_id, status, faulty, batch, batch_qty, auto)
      values (new_id, coalesce(sub->>'unit_id', sub->>'unitId'),
              coalesce(nullif(sub->>'status',''),'stock'),
              coalesce(nullif(sub->>'faulty','')::boolean, false),
              coalesce(nullif(sub->>'batch','')::boolean, false),
              nullif(coalesce(sub->>'batch_qty', sub->>'batchQty'),'')::int,
              coalesce(nullif(sub->>'auto','')::boolean, false))
      on conflict (unit_id) do nothing;
    end loop;

    for sub in select * from jsonb_array_elements(coalesce(r->'comments', '[]'::jsonb)) loop
      insert into public.comments (component_id, author, body, tag)
      values (new_id, coalesce(sub->>'author',''), coalesce(sub->>'body', sub->>'text',''),
              coalesce(nullif(sub->>'tag',''),'General'));
    end loop;
  end loop;

  -- box contents need components to exist first
  for r in select * from jsonb_array_elements(coalesce(p_data->'boxes', '[]'::jsonb)) loop
    for sub in select * from jsonb_array_elements(coalesce(r->'contents', '[]'::jsonb)) loop
      insert into public.box_contents (box_id, component_id, qty)
      select (r->>'id')::uuid, cid, coalesce(nullif(sub->>'qty','')::int, 1)
        from (select coalesce(nullif(sub->>'component_id',''), nullif(sub->>'componentId',''))::uuid cid) x
       where exists (select 1 from public.components where id = x.cid)
      on conflict do nothing;
    end loop;
    for sub in select * from jsonb_array_elements(coalesce(r->'comments', '[]'::jsonb)) loop
      insert into public.comments (box_id, author, body, tag)
      values ((r->>'id')::uuid, coalesce(sub->>'author',''),
              coalesce(sub->>'body', sub->>'text',''), coalesce(nullif(sub->>'tag',''),'General'));
    end loop;
  end loop;

  for r in select * from jsonb_array_elements(coalesce(p_data->'projects', '[]'::jsonb)) loop
    new_id := coalesce(nullif(r->>'id','')::uuid, gen_random_uuid());
    insert into public.projects (id, code, name, description, detail, file_name, status)
    values (new_id,
            coalesce(nullif(r->>'code',''), public.next_code('PRJ','public.project_code_seq')),
            r->>'name', coalesce(r->>'description',''), coalesce(r->>'detail',''),
            coalesce(r->>'file_name', r->>'fileName',''),
            coalesce(nullif(r->>'status',''),'planned'));
    n_proj := n_proj + 1;

    for sub in select * from jsonb_array_elements(coalesce(r->'parts', '[]'::jsonb)) loop
      insert into public.project_parts (project_id, component_id, qty, status)
      select new_id, cid, coalesce(nullif(sub->>'qty','')::int, 1),
             coalesce(nullif(sub->>'status',''),'planned')
        from (select coalesce(nullif(sub->>'component_id',''), nullif(sub->>'id',''))::uuid cid) x
       where exists (select 1 from public.components where id = x.cid)
      on conflict do nothing;
    end loop;

    for sub in select * from jsonb_array_elements(coalesce(r->'comments', '[]'::jsonb)) loop
      insert into public.comments (project_id, author, body, tag)
      values (new_id, coalesce(sub->>'author',''), coalesce(sub->>'body', sub->>'text',''),
              coalesce(nullif(sub->>'tag',''),'Build Note'));
    end loop;
  end loop;

  for r in select * from jsonb_array_elements(coalesce(p_data->'funds', '[]'::jsonb)) loop
    new_id := coalesce(nullif(r->>'id','')::uuid, gen_random_uuid());
    insert into public.funds (id, code, name, provider, kind, currency, event_id, requested,
                              approved, received, applied_on, deadline, status, contact, ref, notes, docs)
    values (new_id,
            coalesce(nullif(r->>'code',''), public.next_code('FND','public.fund_code_seq')),
            r->>'name', coalesce(r->>'provider',''), coalesce(nullif(r->>'kind',''),'Grant'),
            coalesce(nullif(r->>'currency',''),'INR'),
            nullif(coalesce(r->>'event_id', r->>'eventId'),'')::uuid,
            nullif(r->>'requested','')::numeric, nullif(r->>'approved','')::numeric,
            nullif(r->>'received','')::numeric,
            nullif(coalesce(r->>'applied_on', r->>'appliedOn'),'')::date,
            nullif(r->>'deadline','')::date, coalesce(nullif(r->>'status',''),'Draft'),
            coalesce(r->>'contact',''), coalesce(r->>'ref',''), coalesce(r->>'notes',''),
            coalesce(r->>'docs',''));
    n_fund := n_fund + 1;

    insert into public.fund_projects (fund_id, project_id)
    select new_id, pid::uuid
      from jsonb_array_elements_text(coalesce(r->'projectIds', r->'project_ids', '[]'::jsonb)) pid
     where pid <> '' and exists (select 1 from public.projects where id = pid::uuid)
    on conflict do nothing;

    for sub in select * from jsonb_array_elements(coalesce(r->'parts', '[]'::jsonb)) loop
      insert into public.fund_parts (fund_id, component_id, qty)
      select new_id, cid, coalesce(nullif(sub->>'qty','')::int, 1)
        from (select coalesce(nullif(sub->>'component_id',''), nullif(sub->>'id',''))::uuid cid) x
       where exists (select 1 from public.components where id = x.cid)
      on conflict do nothing;
    end loop;

    for sub in select * from jsonb_array_elements(coalesce(r->'history', '[]'::jsonb)) loop
      insert into public.fund_history (fund_id, status, note, created_by)
      values (new_id, coalesce(nullif(sub->>'status',''),'Draft'), coalesce(sub->>'note',''),
              coalesce(sub->>'created_by',''));
    end loop;

    for sub in select * from jsonb_array_elements(coalesce(r->'comments', '[]'::jsonb)) loop
      insert into public.comments (fund_id, author, body, tag)
      values (new_id, coalesce(sub->>'author',''), coalesce(sub->>'body', sub->>'text',''),
              coalesce(nullif(sub->>'tag',''),'Funding Note'));
    end loop;
  end loop;

  for r in select * from jsonb_array_elements(coalesce(p_data->'labels', '[]'::jsonb)) loop
    insert into public.labels (label_id, type, component_id, unit_id, box_id, data, printed)
    select coalesce(r->>'label_id', r->>'labelId'), coalesce(nullif(r->>'type',''),'component'),
           nullif(coalesce(r->>'component_id', r->>'componentId'),'')::uuid,
           nullif(coalesce(r->>'unit_id', r->>'unitId'),'')::uuid,
           nullif(coalesce(r->>'box_id', r->>'boxId'),'')::uuid,
           r->'data', coalesce(nullif(r->>'printed','')::boolean, false)
    on conflict (label_id) do nothing;
  end loop;

  -- reconnect suppliers by name where the id link was lost
  update public.components c set supplier_id = s.id
    from public.suppliers s
   where lower(s.name) = lower(c.supplier_name) and c.supplier_id is null;

  if p_data ? 'settings' then
    update public.settings set
      currency_symbol   = coalesce(p_data->'settings'->>'currency_symbol',
                                   p_data->'settings'->>'currencySymbol', currency_symbol),
      low_stock_default = coalesce(nullif(coalesce(p_data->'settings'->>'low_stock_default',
                                   p_data->'settings'->>'lowStockDefault'),'')::int, low_stock_default),
      comp_prefix       = coalesce(p_data->'settings'->>'comp_prefix',
                                   p_data->'settings'->>'compPrefix', comp_prefix),
      box_prefix        = coalesce(p_data->'settings'->>'box_prefix',
                                   p_data->'settings'->>'boxPrefix', box_prefix),
      date_fmt          = coalesce(p_data->'settings'->>'date_fmt',
                                   p_data->'settings'->>'dateFmt', date_fmt),
      automation        = coalesce(p_data->'settings'->'automation', automation)
    where id = 1;
  end if;

  -- keep the sequences ahead of any restored codes
  perform setval('public.component_code_seq',
    greatest((select coalesce(max((regexp_match(code,'(\d+)$'))[1]::int), 0) from public.components), 1));
  perform setval('public.box_code_seq',
    greatest((select coalesce(max((regexp_match(code,'(\d+)$'))[1]::int), 0) from public.boxes), 1));
  perform setval('public.project_code_seq',
    greatest((select coalesce(max((regexp_match(code,'(\d+)$'))[1]::int), 0) from public.projects), 1));
  perform setval('public.fund_code_seq',
    greatest((select coalesce(max((regexp_match(code,'(\d+)$'))[1]::int), 0) from public.funds), 1));

  insert into public.activity (body, glyph, color, actor, entity_type)
  values ('Store restored from backup', '⟳', '#8da2c8', p_actor, 'system');

  return jsonb_build_object('suppliers', n_sup, 'events', n_evt, 'boxes', n_box,
                            'components', n_comp, 'projects', n_proj, 'funds', n_fund);
end $$;

-- ---------------------------------------------------------------------------
-- global_search(query, limit)
--   → grouped results across components, projects, suppliers, boxes, units.
--     Backs GET /api/search.
-- ---------------------------------------------------------------------------
create or replace function public.global_search(p_q text, p_limit integer default 8)
returns jsonb language sql stable as $$
  with q as (select '%' || lower(coalesce(p_q,'')) || '%' as t)
  select jsonb_build_object(
    'query', p_q,
    'components', coalesce((select jsonb_agg(jsonb_build_object(
        'id', c.id, 'code', c.code, 'name', c.name, 'category', c.category,
        'location', c.location, 'quantity', c.quantity))
      from (select * from public.components c, q
             where lower(c.name) like q.t or lower(c.code) like q.t
                or lower(c.category) like q.t or lower(c.location) like q.t
             order by c.name limit p_limit) c), '[]'::jsonb),
    'projects', coalesce((select jsonb_agg(jsonb_build_object(
        'id', p.id, 'code', p.code, 'name', p.name, 'status', p.status))
      from (select * from public.projects p, q
             where lower(p.name) like q.t or lower(p.code) like q.t or lower(p.description) like q.t
             order by p.name limit p_limit) p), '[]'::jsonb),
    'suppliers', coalesce((select jsonb_agg(jsonb_build_object(
        'id', s.id, 'name', s.name, 'email', s.email))
      from (select * from public.suppliers s, q
             where lower(s.name) like q.t or lower(s.email) like q.t
             order by s.name limit p_limit) s), '[]'::jsonb),
    'boxes', coalesce((select jsonb_agg(jsonb_build_object(
        'id', b.id, 'code', b.code, 'name', b.name, 'location', b.location))
      from (select * from public.boxes b, q
             where lower(b.name) like q.t or lower(b.code) like q.t or lower(b.location) like q.t
             order by b.name limit p_limit) b), '[]'::jsonb),
    'units', coalesce((select jsonb_agg(jsonb_build_object(
        'id', u.id, 'unitId', u.unit_id, 'status', u.status, 'componentId', u.component_id))
      from (select * from public.component_units u, q
             where lower(u.unit_id) like q.t
             order by u.unit_id limit p_limit) u), '[]'::jsonb)
  );
$$;

-- ---------------------------------------------------------------------------
-- purge_expired_sessions() — called opportunistically by the Go backend
-- ---------------------------------------------------------------------------
create or replace function public.purge_expired_sessions()
returns integer language plpgsql as $$
declare n integer;
begin
  delete from public.sessions where expires_at < now();
  get diagnostics n = row_count;
  return n;
end $$;

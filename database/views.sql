-- ===========================================================================
-- Electronic Store Manager — report views
-- Run after functions.sql. Idempotent.
--
-- The Go reports service (backend/services/reports.go) selects from these and
-- shapes the JSON; no report logic is duplicated in Go SQL strings.
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- Stock, with unit rollups and the low-stock flag
-- ---------------------------------------------------------------------------
create or replace view public.v_component_stock as
select c.id, c.code, c.name, c.category, c.location,
       c.quantity, c.min_stock, c.reorder_point, c.price,
       c.faulty, c.unit_tracked, c.supplier_id,
       coalesce(s.name, nullif(c.supplier_name, '')) as supplier,
       round(coalesce(c.price, 0) * c.quantity, 2)   as stock_value,
       count(u.id) filter (where u.status = 'stock')    as units_in_stock,
       count(u.id) filter (where u.status = 'reserved') as units_reserved,
       count(u.id) filter (where u.status = 'in-use')   as units_in_use,
       count(u.id) filter (where u.faulty)              as units_faulty,
       count(u.id)                                      as units_total,
       (c.min_stock is not null and c.quantity <= c.min_stock) as low_stock,
       c.created_at, c.updated_at
  from public.components c
  left join public.suppliers s      on s.id = c.supplier_id
  left join public.component_units u on u.component_id = c.id
 group by c.id, s.name;

-- ---------------------------------------------------------------------------
-- Low stock, with a suggested order quantity and the preferred supplier
-- ---------------------------------------------------------------------------
create or replace view public.v_low_stock as
select v.*,
       greatest(coalesce(v.min_stock, 0) * 2 - v.quantity, 0) as suggested_order,
       coalesce(v.min_stock, 0) - v.quantity                  as deficit,
       round(coalesce(v.price, 0) * greatest(coalesce(v.min_stock, 0) * 2 - v.quantity, 0), 2)
         as suggested_order_cost,
       sup.email as supplier_email,
       sup.phone as supplier_phone
  from public.v_component_stock v
  left join public.suppliers sup on sup.id = v.supplier_id
 where v.low_stock
 order by v.quantity::numeric / nullif(v.min_stock, 0) asc nulls last;

-- ---------------------------------------------------------------------------
-- Valuation by category
-- ---------------------------------------------------------------------------
create or replace view public.v_valuation as
select category,
       count(*)         as skus,
       sum(quantity)    as units,
       sum(stock_value) as value,
       round(avg(nullif(price, 0)), 2) as avg_price
  from public.v_component_stock
 group by category
 order by value desc nulls last;

-- ---------------------------------------------------------------------------
-- Bills of materials, line by line
-- ---------------------------------------------------------------------------
create or replace view public.v_project_bom as
select p.id as project_id, p.code as project_code, p.name as project_name, p.status,
       pp.id as part_id,
       c.id as component_id, c.code as component_code, c.name as component_name,
       c.category, c.location,
       pp.qty, pp.status as part_status, pp.unit_ids,
       c.quantity as on_hand,
       round(coalesce(c.price, 0) * pp.qty, 2) as line_cost,
       (c.quantity >= pp.qty)                  as coverable,
       greatest(pp.qty - c.quantity, 0)        as short_by
  from public.projects p
  join public.project_parts pp on pp.project_id = p.id
  join public.components c     on c.id = pp.component_id;

-- ---------------------------------------------------------------------------
-- One row per project: cost and coverage
-- ---------------------------------------------------------------------------
create or replace view public.v_project_cost as
select project_id, project_code, project_name, status,
       count(*)                              as line_items,
       sum(qty)                              as total_parts,
       sum(line_cost)                        as bom_cost,
       count(*) filter (where not coverable)  as short_lines,
       (count(*) filter (where not coverable) = 0) as buildable
  from public.v_project_bom
 group by project_id, project_code, project_name, status;

-- ---------------------------------------------------------------------------
-- Supplier spend and holdings
-- ---------------------------------------------------------------------------
create or replace view public.v_supplier_spend as
select s.id, s.name, s.email, s.phone, s.website,
       count(c.id)                                     as skus,
       coalesce(sum(c.quantity), 0)                    as units,
       round(coalesce(sum(coalesce(c.price, 0) * c.quantity), 0), 2) as stock_value,
       count(c.id) filter (where c.min_stock is not null and c.quantity <= c.min_stock)
         as low_stock_skus
  from public.suppliers s
  left join public.components c on c.supplier_id = s.id
 group by s.id
 order by stock_value desc nulls last;

-- ---------------------------------------------------------------------------
-- Where a component is used
-- ---------------------------------------------------------------------------
create or replace view public.v_where_used as
select c.id as component_id, c.code, c.name,
       p.id as project_id, p.code as project_code, p.name as project_name,
       p.status as project_status, pp.qty, pp.status as part_status
  from public.components c
  join public.project_parts pp on pp.component_id = c.id
  join public.projects p       on p.id = pp.project_id;

-- ---------------------------------------------------------------------------
-- Funding totals by currency and status
-- ---------------------------------------------------------------------------
create or replace view public.v_fund_totals as
select currency, status,
       count(*)              as records,
       coalesce(sum(requested), 0) as requested,
       coalesce(sum(approved), 0)  as approved,
       coalesce(sum(received), 0)  as received
  from public.funds
 group by currency, status;

-- ---------------------------------------------------------------------------
-- Funding pipeline, with overdue detection
-- ---------------------------------------------------------------------------
create or replace view public.v_fund_pipeline as
select f.*,
       e.name       as event_name,
       e.event_date as event_date,
       (f.deadline is not null and f.deadline < current_date
        and f.status in ('Draft','Applied','Under Review')) as overdue,
       case when f.deadline is null then null
            else f.deadline - current_date end               as days_left,
       coalesce(fp.projects, 0) as linked_projects
  from public.funds f
  left join public.events e on e.id = f.event_id
  left join (select fund_id, count(*) as projects
               from public.fund_projects group by fund_id) fp on fp.fund_id = f.id;

-- ---------------------------------------------------------------------------
-- Box fill, for the bin optimiser
-- ---------------------------------------------------------------------------
create or replace view public.v_box_fill as
select b.id, b.code, b.name, b.location, b.capacity,
       coalesce(sum(k.qty), 0)                       as used,
       b.capacity - coalesce(sum(k.qty), 0)          as free,
       count(k.component_id)                          as distinct_components,
       round(100.0 * coalesce(sum(k.qty), 0) / b.capacity, 1) as pct_full
  from public.boxes b
  left join public.box_contents k on k.box_id = b.id
 group by b.id
 order by pct_full desc;

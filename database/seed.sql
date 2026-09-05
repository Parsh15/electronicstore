-- ===========================================================================
-- Electronic Store Manager — demo seed
-- Run AFTER schema.sql, functions.sql and views.sql. Optional.
-- Re-running wipes and reloads the demo rows. Accounts are NOT touched.
--
-- Passwords are never seeded here: bcrypt hashing belongs to the Go backend.
-- Create the first admin with:
--     cd backend && go run . -create-admin
-- ===========================================================================

begin;

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
delete from public.activity;
delete from public.automation_log;

-- Suppliers ------------------------------------------------------------------
insert into public.suppliers (name, email, phone, website) values
  ('Mouser',     'orders@mouser.in',   '+91 22 4000 1100', 'mouser.in'),
  ('DigiKey',    'india@digikey.com',  '+91 80 4000 2200', 'digikey.in'),
  ('RPi Direct', 'sales@rpidirect.in', '+91 832 240 1100', 'rpidirect.in'),
  ('Generic',    '', '', ''),
  ('Nichicon',   '', '', 'nichicon.co.jp'),
  ('Murata',     '', '', 'murata.com'),
  ('TI',         '', '', 'ti.com'),
  ('Adafruit',   '', '', 'adafruit.com'),
  ('Aosong',     '', '', 'aosong.com'),
  ('Omron',      '', '', 'omron.com'),
  ('JST',        '', '', 'jst.com'),
  ('Infineon',   '', '', 'infineon.com'),
  ('Samsung',    '', '', 'samsungsdi.com'),
  ('Worldsemi',  '', '', 'world-semi.com');

-- Boxes ----------------------------------------------------------------------
insert into public.boxes (name, location, description, capacity) values
  ('MCU & Dev Boards', 'Shelf A1', 'Microcontrollers and dev boards for active builds.', 250),
  ('Sensor Drawer',    'Shelf D2', 'Environmental and distance sensors.',                250),
  ('Power Bin',        'Shelf C1', 'Regulators, MOSFETs and cells.',                     250),
  ('Passives Tray',    'Shelf B2', 'Resistors, capacitors, connectors.',                2000);

-- Components -----------------------------------------------------------------
insert into public.components
  (name, quantity, category, location, min_stock, reorder_point, price, supplier_name, unit_tracked)
values
  ('ATmega328P MCU',          42, 'Microcontrollers', 'A1-03',  20,  20, 245, 'Mouser',     false),
  ('ESP32-WROOM-32',           8, 'Microcontrollers', 'A1-04',  15,  15, 315, 'DigiKey',    true),
  ('Raspberry Pi Pico',       22, 'Microcontrollers', 'A1-07',  20,  20, 330, 'RPi Direct', false),
  ('10k Ohm Resistor 1/4W', 1850, 'Resistors',        'B2-11', 500, 500,   2, 'Generic',    false),
  ('100uF Electrolytic Cap', 120, 'Capacitors',       'B3-07', 100, 100,  15, 'Nichicon',   false),
  ('0.1uF Ceramic Cap',       60, 'Capacitors',       'B3-02', 200, 200,   4, 'Murata',     false),
  ('LM7805 Regulator',        75, 'Power',            'C1-05',  30,  30,  38, 'TI',         false),
  ('SSD1306 OLED 0.96"',      14, 'Displays',         'D4-01',  10,  10, 349, 'Adafruit',   false),
  ('DHT22 Temp/Humidity',      5, 'Sensors',          'D2-09',  12,  15, 540, 'Aosong',     true),
  ('HC-SR04 Ultrasonic',      33, 'Sensors',          'D2-03',  15,  15,  91, 'Generic',    false),
  ('Tactile Push Button',    410, 'Connectors',       'B5-14', 200, 200,   7, 'Omron',      false),
  ('JST-XH 2pin Connector',  220, 'Connectors',       'B5-02', 150, 150,   5, 'JST',        false),
  ('IRF540N MOSFET',          18, 'Power',            'C1-12',  25,  25,  51, 'Infineon',   false),
  ('NE555 Timer IC',          90, 'ICs',              'C2-08',  40,  40,  18, 'TI',         false),
  ('18650 Li-ion Cell',       26, 'Power',            'C3-01',  30,  30, 374, 'Samsung',    false),
  ('WS2812B LED (per m)',     60, 'Displays',         'D4-08',  25,  25, 257, 'Worldsemi',  false);

update public.components c set supplier_id = s.id
  from public.suppliers s where lower(s.name) = lower(c.supplier_name);

-- Tracked units --------------------------------------------------------------
insert into public.component_units (component_id, unit_id, status, faulty)
select c.id,
       c.code || '-U' || lpad(g::text, 3, '0'),
       case when c.name = 'DHT22 Temp/Humidity' and g = 3 then 'faulty' else 'stock' end,
       (c.name = 'DHT22 Temp/Humidity' and g = 3)
  from public.components c
  cross join generate_series(1, 8) g
 where c.unit_tracked
   and (c.name <> 'DHT22 Temp/Humidity' or g <= 5);

-- Box contents ---------------------------------------------------------------
insert into public.box_contents (box_id, component_id, qty)
select b.id, c.id, v.qty
  from (values
    ('MCU & Dev Boards', 'ATmega328P MCU',          42),
    ('MCU & Dev Boards', 'ESP32-WROOM-32',           8),
    ('MCU & Dev Boards', 'Raspberry Pi Pico',       22),
    ('Sensor Drawer',    'DHT22 Temp/Humidity',      5),
    ('Sensor Drawer',    'HC-SR04 Ultrasonic',      33),
    ('Sensor Drawer',    'SSD1306 OLED 0.96"',      14),
    ('Power Bin',        'LM7805 Regulator',        75),
    ('Power Bin',        'IRF540N MOSFET',          18),
    ('Power Bin',        '18650 Li-ion Cell',       26),
    ('Passives Tray',    '100uF Electrolytic Cap', 120),
    ('Passives Tray',    '0.1uF Ceramic Cap',       60),
    ('Passives Tray',    'JST-XH 2pin Connector',  220)
  ) as v(box, comp, qty)
  join public.boxes b      on b.name = v.box
  join public.components c on c.name = v.comp;

-- Projects and BOMs ----------------------------------------------------------
insert into public.projects (name, description, detail, file_name, status, started_at) values
  ('Smart Greenhouse Controller', 'ESP32-based climate + soil monitoring rig',
   'Monitors temperature, humidity and soil moisture; drives fans and a pump via relays.',
   'greenhouse_v3.pdf', 'active', now() - interval '21 days'),
  ('Bench Power Supply Mk2', 'Adjustable 0-30V lab supply',
   'Linear supply with MOSFET pass stage and OLED readout.',
   'psu_mk2_sch.pdf', 'active', now() - interval '9 days'),
  ('LED Matrix Clock', 'WS2812 + RTC desk clock',
   'Pico-driven addressable LED clock with ambient light sensing.', '', 'planned', null);

insert into public.project_parts (project_id, component_id, qty)
select p.id, c.id, v.qty
  from (values
    ('Smart Greenhouse Controller', 'ESP32-WROOM-32',          2),
    ('Smart Greenhouse Controller', 'DHT22 Temp/Humidity',     3),
    ('Smart Greenhouse Controller', 'SSD1306 OLED 0.96"',      1),
    ('Smart Greenhouse Controller', 'LM7805 Regulator',        1),
    ('Smart Greenhouse Controller', '10k Ohm Resistor 1/4W',   6),
    ('Bench Power Supply Mk2',      'LM7805 Regulator',        2),
    ('Bench Power Supply Mk2',      'IRF540N MOSFET',          4),
    ('Bench Power Supply Mk2',      '100uF Electrolytic Cap',  8),
    ('Bench Power Supply Mk2',      '18650 Li-ion Cell',       4),
    ('LED Matrix Clock',            'Raspberry Pi Pico',       1),
    ('LED Matrix Clock',            'WS2812B LED (per m)',     4),
    ('LED Matrix Clock',            'Tactile Push Button',     3),
    ('LED Matrix Clock',            'NE555 Timer IC',          1)
  ) as v(proj, comp, qty)
  join public.projects p   on p.name = v.proj
  join public.components c on c.name = v.comp;

-- Comments -------------------------------------------------------------------
insert into public.comments (component_id, author, body, tag)
select c.id, v.author, v.body, v.tag
  from (values
    ('DHT22 Temp/Humidity', 'Marco', 'Unit U003 reads open-circuit — pulled from the shelf.', 'Faulty Note'),
    ('DHT22 Temp/Humidity', 'Priya', 'Whole batch runs ~2°C high; apply firmware offset.',    'General'),
    ('IRF540N MOSFET',      'Dana',  'Two from this lot failed Vgs test.',                    'Faulty Note'),
    ('Tactile Push Button', 'Sam',   'Restocked 200 from Omron order #4471.',                 'Restock Note')
  ) as v(comp, author, body, tag)
  join public.components c on c.name = v.comp;

insert into public.comments (project_id, author, body, tag)
select p.id, v.author, v.body, 'Build Note'
  from (values
    ('Smart Greenhouse Controller', 'Priya', 'Switched to two ESP32s for redundancy on the pump controller.'),
    ('Smart Greenhouse Controller', 'Marco', 'DHT22 batch runs ~2C high, needs a calibration offset in firmware.'),
    ('Bench Power Supply Mk2',      'Dana',  'Two of the IRF540N from stock tested faulty — pulled from the build.'),
    ('LED Matrix Clock',            'Sam',   'Power budget tight at full brightness — added current limiting.')
  ) as v(proj, author, body)
  join public.projects p on p.name = v.proj;

insert into public.comments (box_id, author, body, tag)
select b.id, 'Dana', 'Contains the faulty MOSFET lot — flagged.', 'Faulty Note'
  from public.boxes b where b.name = 'Power Bin';

-- Events and funding ---------------------------------------------------------
insert into public.events (name, org, type, event_date, venue) values
  ('Goa Innovation Challenge 2026', 'Goa State Innovation Council', 'Competition',
   current_date + 45, 'Panaji'),
  ('MSME Design Grant — Cycle 3',   'Ministry of MSME',             'Grant Cycle',
   current_date + 90, 'Online');

insert into public.funds (name, provider, kind, currency, event_id, requested, approved,
                          received, applied_on, deadline, status, contact, ref)
select v.name, v.provider, v.kind, v.currency, e.id,
       v.requested, v.approved, v.received, v.applied_on, v.deadline, v.status, v.contact, v.ref
  from (values
    ('Greenhouse Pilot Grant', 'Goa State Innovation Council', 'Grant', 'INR',
     'Goa Innovation Challenge 2026', 450000::numeric, 300000::numeric, 150000::numeric,
     current_date - 30, current_date + 45, 'Approved', 'Dr. R. Naik', 'GSIC/26/0114'),
    ('MSME Design Support', 'Ministry of MSME', 'Grant', 'INR',
     'MSME Design Grant — Cycle 3', 250000::numeric, null::numeric, null::numeric,
     current_date - 7, current_date + 90, 'Under Review', 'MSME Desk', 'MSME/DG/3/882'),
    ('Bench Equipment Prize', 'Maker Fest Goa', 'Competition Prize', 'INR',
     null, 75000::numeric, 75000::numeric, 75000::numeric,
     current_date - 120, null, 'Received', '', 'MFG/2026/17')
  ) as v(name, provider, kind, currency, event, requested, approved, received,
         applied_on, deadline, status, contact, ref)
  left join public.events e on e.name = v.event;

insert into public.fund_projects (fund_id, project_id)
select f.id, p.id
  from (values
    ('Greenhouse Pilot Grant', 'Smart Greenhouse Controller'),
    ('MSME Design Support',    'Bench Power Supply Mk2'),
    ('Bench Equipment Prize',  'Bench Power Supply Mk2')
  ) as v(fund, proj)
  join public.funds f    on f.name = v.fund
  join public.projects p on p.name = v.proj;

insert into public.fund_history (fund_id, status, note, created_by, created_at)
select f.id, v.status, v.note, 'Aditya', now() - (v.days || ' days')::interval
  from (values
    ('Greenhouse Pilot Grant', 'Draft',    'Fund record created',        40),
    ('Greenhouse Pilot Grant', 'Applied',  'Submitted with BOM annex',   30),
    ('Greenhouse Pilot Grant', 'Approved', 'Partial award ₹3,00,000',      8),
    ('MSME Design Support',    'Draft',    'Fund record created',        10),
    ('MSME Design Support',    'Applied',  'Portal submission complete',  7),
    ('Bench Equipment Prize',  'Received', 'Prize money credited',       95)
  ) as v(fund, status, note, days)
  join public.funds f on f.name = v.fund;

insert into public.activity (body, glyph, color, actor, entity_type)
values ('Store initialised with seed inventory', '⚡', '#8da2c8', 'system', 'system');

commit;

-- Sanity check
select 'suppliers'  as t, count(*) from public.suppliers
union all select 'components', count(*) from public.components
union all select 'units',      count(*) from public.component_units
union all select 'boxes',      count(*) from public.boxes
union all select 'box lines',  count(*) from public.box_contents
union all select 'projects',   count(*) from public.projects
union all select 'bom lines',  count(*) from public.project_parts
union all select 'funds',      count(*) from public.funds
union all select 'events',     count(*) from public.events
union all select 'comments',   count(*) from public.comments;

/* Electronic Store Manager — data layer
   -----------------------------------------------------------------------------
   Every read and write goes to the Go API over fetch with `credentials:
   'include'`. There is no database URL, no key, and no SQL in this file — the
   browser cannot reach Postgres and does not need to.

   Offline behaviour is unchanged: reads fall back to the localStorage cache,
   writes are queued, and the queue is flushed the next time a call succeeds.
   The server wins on conflict, which is the same rule the app has always used.

   Shapes are the ones the UI already expects (camelCase, `text` for a comment
   body, epoch-millisecond timestamps). That translation happens in the API's
   SQL, so nothing has to be remapped here.                                    */

import { api } from './esm-auth.js';

const CACHE_KEY = 'esm_data_v4';
const QUEUE_KEY = 'esm_sync_queue';
const MARK_KEY  = 'esm_last_sync';

const jget = (k, d) => { try { const r = localStorage.getItem(k); return r ? JSON.parse(r) : d; } catch (e) { return d; } };
const jset = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) {} };

const qs = (params) => {
  const out = [];
  Object.keys(params || {}).forEach((k) => {
    const v = params[k];
    if (v !== undefined && v !== null && v !== '' && v !== false) {
      out.push(encodeURIComponent(k) + '=' + encodeURIComponent(v));
    }
  });
  return out.length ? '?' + out.join('&') : '';
};

const get  = (path, params) => api(path + qs(params));
const post = (path, body) => api(path, { method: 'POST', body: JSON.stringify(body || {}) });
const put  = (path, body) => api(path, { method: 'PUT',  body: JSON.stringify(body || {}) });
const del  = (path) => api(path, { method: 'DELETE' });

/* ---------------------------------------------------------------- write queue

   A write attempted while offline is stored and replayed later. Only network
   failures are queued: a 400 means the server rejected the request on its
   merits, and retrying it forever would never help.                          */

function enqueue(job) {
  const q = jget(QUEUE_KEY, []);
  q.push(Object.assign({ at: Date.now() }, job));
  jset(QUEUE_KEY, q.slice(-500));
}

async function drain() {
  const q = jget(QUEUE_KEY, []);
  if (!q.length) return 0;
  const left = [];
  let done = 0;
  for (const job of q) {
    try {
      await api(job.path, { method: job.method, body: job.body });
      done++;
    } catch (e) {
      if (e.offline) left.push(job);   // still offline: keep it for next time
    }
  }
  jset(QUEUE_KEY, left);
  return done;
}

// Wraps a write so it survives a dropped connection.
async function guarded(method, path, body) {
  const payload = JSON.stringify(body || {});
  try {
    return await api(path, { method, body: payload });
  } catch (e) {
    if (e.offline) { enqueue({ method, path, body: payload }); return null; }
    throw e;
  }
}

/* --------------------------------------------------------------------- facade */

export const DB = {
  get pending()  { return jget(QUEUE_KEY, []).length; },
  get lastSync() { return jget(MARK_KEY, 0); },
  get cached()   { return jget(CACHE_KEY, null); },

  retry: drain,

  /* --- whole store, one object ------------------------------------------

     The app keeps its state as a single object and always has. These two calls
     preserve that: the server is the shared source of truth, localStorage is
     the offline cache, and the UI is untouched.                            */

  /** The whole store, in the shape the app's state already uses. */
  async pullState() {
    const state = await get('/api/sync');
    jset(CACHE_KEY, state);
    jset(MARK_KEY, Date.now());
    return state;
  },

  /** Replaces the whole store. Queued if offline, so no save is ever lost. */
  async pushState(state) {
    jset(CACHE_KEY, state);
    const res = await guarded('PUT', '/api/sync', state);
    if (res) jset(MARK_KEY, Date.now());
    return res;
  },

  /* --- whole store ------------------------------------------------------ */

  /** Everything the app needs on load, in one pass. Falls back to the cache. */
  async pull() {
    try {
      await drain();
      const [components, projects, boxes, funds, events, suppliers, activity, settings, trash] =
        await Promise.all([
          get('/api/components'),
          get('/api/projects'),
          get('/api/boxes'),
          get('/api/funds'),
          get('/api/events'),
          get('/api/suppliers'),
          get('/api/activity', { limit: 100 }),
          get('/api/settings'),
          get('/api/trash'),
        ]);
      const state = { components, projects, boxes, funds, events, suppliers, activity, settings, trash };
      jset(CACHE_KEY, state);
      jset(MARK_KEY, Date.now());
      return { source: 'server', state };
    } catch (e) {
      const cached = jget(CACHE_KEY, null);
      if (cached) return { source: 'cache', state: cached, error: e.message };
      throw e;
    }
  },

  /* --- components ------------------------------------------------------- */

  components:      (f) => get('/api/components', f),
  component:       (id) => get('/api/components/' + id),
  createComponent: (c) => guarded('POST', '/api/components', c),
  updateComponent: (id, c) => guarded('PUT', '/api/components/' + id, c),
  deleteComponent: (id) => del('/api/components/' + id),
  restock:         (id, add, note) => guarded('POST', '/api/components/' + id + '/restock', { add, note }),
  duplicateComponent: (id) => post('/api/components/' + id + '/duplicate'),
  componentUnits:  (id) => get('/api/components/' + id + '/units'),
  whereUsed:       (id) => get('/api/components/' + id + '/where-used'),
  componentHistory:(id) => get('/api/components/' + id + '/history'),
  componentComments: (id) => get('/api/components/' + id + '/comments'),
  addComponentComment: (id, text, tag) => guarded('POST', '/api/components/' + id + '/comments', { text, tag }),
  lowStock:        () => get('/api/components/low-stock'),
  faulty:          () => get('/api/components/faulty'),
  importComponents: (rows, opts) => post('/api/components/import', Object.assign({ rows }, opts || {})),
  exportComponentsURL: () => '/api/components/export',

  /* --- units ------------------------------------------------------------ */

  units:        (f) => get('/api/units', f),
  unit:         (id) => get('/api/units/' + id),
  updateUnit:   (id, patch) => guarded('PUT', '/api/units/' + id, patch),
  setUnitStatus:(id, status, projectId, note) =>
                  guarded('PUT', '/api/units/' + id + '/status', { status, projectId, note }),
  reserveUnit:  (id, projectId) => post('/api/units/' + id + '/reserve', { projectId }),
  bulkUnitStatus:(unitIds, status, projectId) =>
                  post('/api/units/bulk-status', { unitIds, status, projectId }),
  unitHistory:  (id) => get('/api/units/' + id + '/history'),

  /* --- projects --------------------------------------------------------- */

  projects:      (f) => get('/api/projects', f),
  project:       (id) => get('/api/projects/' + id),
  createProject: (p) => guarded('POST', '/api/projects', p),
  updateProject: (id, p) => guarded('PUT', '/api/projects/' + id, p),
  deleteProject: (id) => del('/api/projects/' + id),
  completeProject: (id) => post('/api/projects/' + id + '/complete'),
  reserveProjectUnits: (id) => post('/api/projects/' + id + '/reserve-units'),
  bom:           (id) => get('/api/projects/' + id + '/bom'),
  saveBOM:       (id, parts, replace) => post('/api/projects/' + id + '/bom', { parts, replace: replace !== false }),
  updateBOMLine: (id, partId, patch) => put('/api/projects/' + id + '/bom/' + partId, patch),
  deleteBOMLine: (id, partId) => del('/api/projects/' + id + '/bom/' + partId),
  addProjectComment: (id, text, tag) => guarded('POST', '/api/projects/' + id + '/comments', { text, tag: tag || 'Build Note' }),

  /* --- suppliers -------------------------------------------------------- */

  suppliers:      (f) => get('/api/suppliers', f),
  supplier:       (id) => get('/api/suppliers/' + id),
  createSupplier: (s) => guarded('POST', '/api/suppliers', s),
  updateSupplier: (id, s) => guarded('PUT', '/api/suppliers/' + id, s),
  deleteSupplier: (id) => del('/api/suppliers/' + id),
  supplierComponents: (id) => get('/api/suppliers/' + id + '/components'),
  supplierSpend:  (id) => get('/api/suppliers/' + id + '/spend'),

  /* --- boxes ------------------------------------------------------------ */

  boxes:       (f) => get('/api/boxes', f),
  box:         (id) => get('/api/boxes/' + id),
  createBox:   (b) => guarded('POST', '/api/boxes', b),
  updateBox:   (id, b) => guarded('PUT', '/api/boxes/' + id, b),
  deleteBox:   (id) => del('/api/boxes/' + id),
  boxContents: (id) => get('/api/boxes/' + id + '/contents'),
  assignToBox: (id, componentId, qty) => post('/api/boxes/' + id + '/assign', { componentId, qty }),
  removeFromBox: (id, componentId) => post('/api/boxes/' + id + '/assign', { componentId, remove: true }),
  optimiseBoxes: () => put('/api/boxes/optimize'),
  addBoxComment: (id, text, tag) => guarded('POST', '/api/boxes/' + id + '/comments', { text, tag }),

  /* --- labels ----------------------------------------------------------- */

  labels:         (f) => get('/api/labels', f),
  label:          (id) => get('/api/labels/' + id),
  generateLabels: (spec, withPNG) => post('/api/labels/generate' + (withPNG ? '?png=true' : ''), spec),
  deleteLabel:    (id) => del('/api/labels/' + id),
  queueLabels:    (labelIds, printed) => post('/api/labels/print-queue', { labelIds, printed: printed !== false }),
  labelsByComponent: (id) => get('/api/labels/by-component/' + id),
  labelsByUnit:   (id) => get('/api/labels/by-unit/' + id),
  labelsByBox:    (id) => get('/api/labels/by-box/' + id),

  /* --- events and funding ----------------------------------------------- */

  events:      (f) => get('/api/events', f),
  event:       (id) => get('/api/events/' + id),
  createEvent: (e) => guarded('POST', '/api/events', e),
  updateEvent: (id, e) => guarded('PUT', '/api/events/' + id, e),
  deleteEvent: (id) => del('/api/events/' + id),

  funds:       (f) => get('/api/funds', f),
  fund:        (id) => get('/api/funds/' + id),
  createFund:  (f) => guarded('POST', '/api/funds', f),
  updateFund:  (id, f) => guarded('PUT', '/api/funds/' + id, f),
  deleteFund:  (id) => del('/api/funds/' + id),
  advanceFund: (id, status, note) => guarded('POST', '/api/funds/' + id + '/advance', { status, note }),
  fundHistory: (id) => get('/api/funds/' + id + '/history'),
  fundTotals:  () => get('/api/funds/totals'),
  fundPipeline:() => get('/api/funds/pipeline'),
  addFundComment: (id, text) => guarded('POST', '/api/funds/' + id + '/comments', { text, tag: 'Funding Note' }),

  /* --- reports ---------------------------------------------------------- */

  reports:        () => get('/api/reports'),
  report:         (id) => get('/api/reports/' + id),
  deleteReport:   (id) => del('/api/reports/' + id),
  generateReport: (spec) => post('/api/reports/generate', spec),
  inventoryReport:() => get('/api/reports/inventory'),
  lowStockReport: () => get('/api/reports/low-stock'),
  valuationReport:() => get('/api/reports/valuation'),
  bomReport:      (projectId) => get('/api/reports/bom', { projectId }),
  supplierReport: () => get('/api/reports/supplier'),
  auditReport:    (from, to, userId) => get('/api/reports/audit', { from, to, userId }),

  /** A download URL for a report in CSV or PDF. Cookies ride along on a
      same-site navigation, so this can be used as a plain link or href. */
  reportURL(type, format, params) {
    return '/api/reports/' + type + qs(Object.assign({ format: format || 'csv' }, params || {}));
  },
  savedReportURL(id, format) {
    return '/api/reports/' + id + '/download' + qs({ format: format || 'csv' });
  },

  /* --- activity, search, voice ------------------------------------------ */

  activity:    (f) => get('/api/activity', f),
  logActivity: (body, glyph, color, entityType, entityId) =>
                 guarded('POST', '/api/activity', { body, glyph, color, entityType, entityId }),
  activityExportURL: () => '/api/activity/export',
  search:      (q, limit) => get('/api/search', { q, limit }),
  logVoice:    (command, action, success) => guarded('POST', '/api/voice/log', { command, action, success }),

  /* --- settings and automation ------------------------------------------ */

  settings:         () => get('/api/settings'),
  saveSettings:     (patch) => put('/api/settings', patch),
  automationConfig: () => get('/api/settings/automation'),
  saveAutomation:   (patch) => put('/api/settings/automation', patch),
  automationLog:    (f) => get('/api/automation/log', f),
  automationPlan:   (componentId) => get('/api/automation/plan/' + componentId),
  runAutomation:    (componentId, all) => post('/api/automation/run', { componentId, all }),

  /* --- trash ------------------------------------------------------------ */

  trash:        (f) => get('/api/trash', f),
  restoreTrash: (tid) => post('/api/trash/' + tid + '/restore'),
  purgeTrash:   (tid) => del('/api/trash/' + tid),
  emptyTrash:   () => del('/api/trash/empty'),

  /* --- users (admin) ----------------------------------------------------- */

  users:      () => get('/api/users'),
  user:       (id) => get('/api/users/' + id),
  createUser: (u) => post('/api/users', u),
  updateUser: (id, patch) => put('/api/users/' + id, patch),
  deleteUser: (id) => del('/api/users/' + id),
  setUserRole:(id, role) => put('/api/users/' + id + '/role', { role }),
  activateUser:  (id) => put('/api/users/' + id + '/activate'),
  deactivateUser:(id) => put('/api/users/' + id + '/deactivate'),

  /* --- backup (admin) ---------------------------------------------------- */

  backupURL:     () => '/api/backup/create',
  backupMeta:    () => get('/api/backup/meta'),
  backupHistory: () => get('/api/backup/history'),

  /** Creates a backup and hands back the parsed JSON, for the in-app
      "download a copy" button that builds its own Blob. */
  async createBackup() { return post('/api/backup/create'); },

  /** Replaces the whole store. Admin only; all or nothing on the server. */
  async restoreBackup(data) {
    const res = await post('/api/backup/restore', { data });
    jset(MARK_KEY, Date.now());
    return res;
  },

  /* --- health ------------------------------------------------------------ */

  async health() {
    try {
      const h = await get('/api/health');
      return Object.assign({ pending: DB.pending }, h);
    } catch (e) {
      return { status: 'offline', error: e.message, pending: DB.pending };
    }
  },
};

export default DB;

/* Electronic Store Manager — authentication
   -----------------------------------------------------------------------------
   Same public surface as before: Auth.signIn, Auth.restore, Auth.signOut,
   Auth.listUsers, Auth.createUser, Auth.updateUser, Auth.setPassword,
   Auth.deleteUser, Auth.ensureOwner, Auth.cachedUsers. Nothing in the UI needs
   to change.

   What changed underneath: every call now goes to the Go API over fetch with
   `credentials: 'include'`. The session is an HttpOnly cookie the browser
   attaches on its own — no token is readable from JavaScript, and nothing
   sensitive is written to localStorage. There are no database keys in this
   file, because the browser no longer talks to the database.

   Local mode still exists, unchanged, for a machine with no backend
   configured: accounts live in localStorage and the app works offline exactly
   as it always did.                                                           */

const API_KEY    = 'esm_api_url';
const LOCAL_USERS = 'esm_users';
const LOCAL_SESS  = 'esm_session';
const CACHE_KEY   = 'esm_users_cache';   // display-only mirror of the roster

const jget = (k, d) => { try { const r = localStorage.getItem(k); return r ? JSON.parse(r) : d; } catch (e) { return d; } };
const jset = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) {} };
const jdel = (k) => { try { localStorage.removeItem(k); } catch (e) {} };

const normEmail = (e) => String(e == null ? '' : e).trim().toLowerCase();

const OWNER = { email: 'gooliparshuram16@gmail.com', password: '12345678', name: 'parshuram', role: 'admin' };

/* ------------------------------------------------------------ API base URL

   Resolution order:
     1. ?api=https://... in the URL — set once, remembered, so a deployed build
        can be pointed at a backend without rebuilding it
     2. window.ESM_API_URL, injected by the host page at deploy time
     3. a URL saved in Settings → Cloud
     4. same origin, when the frontend is served behind the same domain
   An empty result means "no backend" and the app runs in local mode.          */

function fromQuery() {
  try {
    const m = /[?&]api=([^&]+)/.exec(location.search);
    if (!m) return null;
    const url = decodeURIComponent(m[1]).trim().replace(/\/+$/, '');
    if (url) { jset(API_KEY, url); return url; }
  } catch (e) {}
  return null;
}

function apiBase() {
  const queried = fromQuery();
  if (queried) return queried;
  const injected = typeof window !== 'undefined' && window.ESM_API_URL;
  const saved = jget(API_KEY, null);
  const url = String(injected || saved || '').trim().replace(/\/+$/, '');
  if (url) return url;
  if (typeof location !== 'undefined' && /^https?:/.test(location.protocol)) return '';
  return null;   // file:// — nothing to talk to
}

const configured = () => apiBase() !== null;

/* ------------------------------------------------------------------ transport

   One place builds every request, so `credentials: 'include'` can never be
   forgotten — without it the cookie is not sent and the API sees a stranger. */

async function api(path, opts = {}) {
  const base = apiBase();
  if (base === null) throw new Error('No server is configured.');

  let res;
  try {
    res = await fetch(base + path, {
      method: opts.method || 'GET',
      credentials: 'include',
      headers: Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {}),
      body: opts.body,
      signal: opts.signal,
    });
  } catch (e) {
    const err = new Error('Cannot reach the server.');
    err.offline = true;
    throw err;
  }

  const text = await res.text();
  let body = null;
  if (text) { try { body = JSON.parse(text); } catch (e) { body = text; } }

  if (!res.ok) {
    const err = new Error((body && body.error) || 'Request failed (' + res.status + ').');
    err.status = res.status;
    err.field = body && body.field;
    throw err;
  }
  return body;
}

export { api };

/* ------------------------------------------------------- local-only password

   Kept so local mode still works and so any account created before the backend
   existed can still sign in on that machine. The server never sees these; it
   uses bcrypt.                                                                */

const cryptoOk = () => typeof crypto !== 'undefined' && crypto.subtle && typeof TextEncoder !== 'undefined';

export async function hashPassword(pw, salt, alg) {
  const input = salt + ':' + String(pw == null ? '' : pw);
  const want = alg || (cryptoOk() ? 'sha256' : 'js');
  if (want === 'sha256' && cryptoOk()) {
    const buf = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(input));
    return Array.from(new Uint8Array(buf)).map((b) => b.toString(16).padStart(2, '0')).join('');
  }
  let h = 5381;
  for (let i = 0; i < input.length; i++) h = ((h << 5) + h + input.charCodeAt(i)) | 0;
  return 'js' + (h >>> 0).toString(16);
}

export async function verifyPassword(u, pw) {
  if (!u || !u.hash) return false;
  const cands = [String(pw == null ? '' : pw)];
  for (const c of cands) {
    if (await hashPassword(c, u.salt, u.alg) === u.hash) return true;
  }
  return false;
}

async function makeCredential(password) {
  const salt = Math.random().toString(36).slice(2, 10);
  const alg = cryptoOk() ? 'sha256' : 'js';
  return { salt, alg, hash: await hashPassword(password, salt, alg) };
}

/* ---------------------------------------------------------------- local mode */

const Local = {
  mode: 'local',

  listRaw() {
    const list = jget(LOCAL_USERS, []);
    if (!Array.isArray(list)) return [];
    return list.filter((u) => u && u.email).map((u) => ({ alg: 'sha256', active: true, ...u, email: normEmail(u.email) }));
  },
  write(list) {
    const clean = (list || []).filter((u) => u && u.email).map((u) => ({ ...u, email: normEmail(u.email) }));
    jset(LOCAL_USERS, clean);
    return clean;
  },
  // Every write re-reads storage first: a stale in-memory snapshot can never
  // wipe an account created in another tab or by an earlier async call.
  mutate(fn) { return Local.write(fn(Local.listRaw())); },
  find(email) { const e = normEmail(email); return Local.listRaw().find((u) => u.email === e) || null; },

  async ensureOwner(defaults) {
    if (Local.listRaw().some((u) => u.email === OWNER.email)) return;
    const c = await makeCredential(OWNER.password);
    Local.mutate((l) => l.concat([{
      id: 'u_owner', name: OWNER.name, email: OWNER.email, role: 'admin',
      active: true, createdBy: 'system', createdAt: Date.now(), ...c, ...(defaults || {}),
    }]));
  },

  async listUsers() { return Local.listRaw(); },

  async createUser({ name, email, password, role, perms, createdBy }) {
    email = normEmail(email);
    if (Local.find(email)) throw new Error('A user with that email already exists.');
    const c = await makeCredential(password);
    const user = {
      id: 'u_' + Math.random().toString(36).slice(2, 10),
      name, email, role: role || 'staff', perms: perms || null,
      active: true, createdBy: createdBy || '', createdAt: Date.now(), ...c,
    };
    Local.mutate((l) => l.concat([user]));
    return user;
  },

  async updateUser(id, patch) {
    const list = Local.mutate((l) => l.map((u) => u.id === id ? { ...u, ...patch } : u));
    return list.find((u) => u.id === id) || null;
  },

  async setPassword(id, password) {
    const u = Local.listRaw().find((x) => x.id === id);
    if (!u) throw new Error('User not found.');
    const c = await makeCredential(password);
    const list = Local.mutate((l) => l.map((x) => x.id === id ? { ...x, ...c } : x));
    return list.find((x) => x.id === id) || null;
  },

  async deleteUser(id) { Local.mutate((l) => l.filter((u) => u.id !== id)); },

  async signIn(email, password) {
    email = normEmail(email);
    const u = Local.listRaw().find((x) => x.email === email);
    if (!u) throw new Error('No account found with that email.');
    if (u.active === false) throw new Error('This account is deactivated. Ask an admin to reactivate it.');
    if (!await verifyPassword(u, password)) throw new Error('Incorrect password.');
    jset(LOCAL_SESS, { email: u.email, at: Date.now() });
    return u;
  },

  // Re-validated against the store on every restore, so a deleted or
  // deactivated account cannot survive a refresh.
  async restore() {
    const sess = jget(LOCAL_SESS, null);
    if (!sess || !sess.email) return null;
    const u = Local.find(sess.email);
    if (!u || u.active === false) { jdel(LOCAL_SESS); return null; }
    return u;
  },

  async signOut() { jdel(LOCAL_SESS); },

  async ping() { return true; },
};

/* ----------------------------------------------------------------- API mode */

const Remote = {
  mode: 'api',

  // Normalises a profile row into the shape the UI has always used.
  _toUser(p) {
    if (!p) return null;
    return {
      id: p.id,
      name: p.name || '',
      email: normEmail(p.email),
      role: p.role || 'staff',
      active: p.active !== false,
      perms: p.perms || null,
      createdBy: p.createdBy || '',
      createdAt: p.createdAt || Date.now(),
      lastLoginAt: p.lastLoginAt || null,
      activeSessions: p.activeSessions,
    };
  },

  async signIn(email, password) {
    const p = await api('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email: normEmail(email), password: String(password) }),
    });
    return Remote._toUser(p);
  },

  // The whole returning-user flow: the browser sends the cookie, the server
  // resolves it to a profile. A 401 simply means "show the sign-in screen".
  async restore() {
    try {
      return Remote._toUser(await api('/api/auth/me'));
    } catch (e) {
      if (e.offline) return null;   // offline: the app falls back to its cache
      return null;
    }
  },

  async signOut() {
    try { await api('/api/auth/logout', { method: 'POST' }); } catch (e) {}
    jdel(CACHE_KEY);
  },

  async refresh() {
    try { await api('/api/auth/refresh', { method: 'POST' }); return true; } catch (e) { return false; }
  },

  async changePassword(currentPassword, newPassword) {
    return api('/api/auth/change-password', {
      method: 'POST',
      body: JSON.stringify({ currentPassword, newPassword }),
    });
  },

  async signUp({ name, email, password }) {
    const p = await api('/api/auth/signup', {
      method: 'POST',
      body: JSON.stringify({ name, email: normEmail(email), password }),
    });
    return Remote._toUser(p);
  },

  async listUsers() {
    const rows = await api('/api/users');
    const users = (Array.isArray(rows) ? rows : []).map(Remote._toUser);
    jset(CACHE_KEY, users);       // so the roster still renders while offline
    return users;
  },

  async createUser(p) {
    return Remote._toUser(await api('/api/users', {
      method: 'POST',
      body: JSON.stringify({
        name: p.name, email: normEmail(p.email), password: p.password,
        role: p.role || 'staff', perms: p.perms || null,
      }),
    }));
  },

  async updateUser(id, patch) {
    return Remote._toUser(await api('/api/users/' + id, {
      method: 'PUT', body: JSON.stringify(patch),
    }));
  },

  async setPassword(id, password) {
    return Remote._toUser(await api('/api/users/' + id, {
      method: 'PUT', body: JSON.stringify({ password }),
    }));
  },

  async deleteUser(id) { await api('/api/users/' + id, { method: 'DELETE' }); },

  async setActive(id, active) {
    return Remote._toUser(await api('/api/users/' + id + (active ? '/activate' : '/deactivate'), {
      method: 'PUT', body: '{}',
    }));
  },

  async setRole(id, role) {
    return Remote._toUser(await api('/api/users/' + id + '/role', {
      method: 'PUT', body: JSON.stringify({ role }),
    }));
  },

  // Accounts are created through User Management or the backend's
  // -create-admin command; there is nothing to seed from the browser.
  async ensureOwner() {},

  async ping() {
    const h = await api('/api/health');
    if (!h || !h.database) throw new Error('The server is up but cannot reach the database.');
    if (!h.schema) throw new Error('The database is reachable but the schema has not been installed.');
    return h;
  },
};

/* ------------------------------------------------------------------ facade */

export const Auth = {
  get mode() { return configured() ? 'api' : 'local'; },
  get backend() { return configured() ? Remote : Local; },

  config() { const url = apiBase(); return url === null ? null : { url: url || location.origin }; },

  async setConfig(url) {
    const clean = String(url || '').trim().replace(/\/+$/, '');
    if (!clean) throw new Error('Enter the address of your server.');
    jset(API_KEY, clean);
    try {
      await Remote.ping();
    } catch (e) {
      jdel(API_KEY);
      throw e;
    }
    return { url: clean };
  },

  clearConfig() { jdel(API_KEY); jdel(CACHE_KEY); },

  ensureOwner(defaults) { return Auth.backend.ensureOwner(defaults || {}); },
  listUsers() { return Auth.backend.listUsers(); },
  createUser(p) { return Auth.backend.createUser(p); },
  updateUser(id, patch) { return Auth.backend.updateUser(id, patch); },
  setPassword(id, pw) { return Auth.backend.setPassword(id, pw); },
  deleteUser(id) { return Auth.backend.deleteUser(id); },
  signIn(email, pw) { return Auth.backend.signIn(email, pw); },
  restore() { return Auth.backend.restore(); },
  signOut() { return Auth.backend.signOut(); },
  ping() { return Auth.backend.ping(); },
  cachedUsers() { return configured() ? jget(CACHE_KEY, []) : Local.listRaw(); },

  // Available in API mode only; the UI checks Auth.mode before offering them.
  signUp(p) { return Remote.signUp(p); },
  changePassword(cur, next) { return Remote.changePassword(cur, next); },
  setActive(id, active) { return Remote.setActive(id, active); },
  setRole(id, role) { return Remote.setRole(id, role); },
  refresh() { return Remote.refresh(); },
};

export default Auth;

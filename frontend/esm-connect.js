/* Electronic Store Manager — connection layer
   -----------------------------------------------------------------------------
   Publishes `window.Auth` and `window.DB` so the app's logic class — a classic
   script, which cannot use `import` — can call the Go API.

   It also runs a connection check on load and prints the result to the console
   and to `window.ESM_CONNECTION`. That check is the thing to read first when
   the app looks signed out or empty: it names the exact failure rather than
   leaving a blank screen.

   Deliberately does NOT change any UI. The app renders exactly as before.     */

import Auth, { api } from './esm-auth.js';
import DB from './esm-db.js';

window.Auth = Auth;
window.DB = DB;
window.esmApi = api;

// Also expose the API base the app resolved, so the settings screen and the
// console can both show which backend this page is actually talking to.
window.ESM_RESOLVED_API = (Auth.config() && Auth.config().url) || null;

/* ------------------------------------------------------------ what went wrong

   Four failures look identical from inside the app — a blank screen and an
   apparently signed-out session. They are told apart here.                    */

const DIAGNOSIS = {
  'no-origin':
    'This page is running from a file:// or blob: URL, which has no origin. ' +
    'Browsers refuse to send cookies or pass CORS from there, so the API can ' +
    'never authenticate it. Serve the folder over http:// — locally with ' +
    '`npx serve frontend -l 3000`.',

  unreachable:
    'The API did not answer. Check that ESM_API_URL points at the running Go ' +
    'service and that the service is deployed and awake.',

  cors:
    'The API answered, but the browser blocked the response. ALLOWED_ORIGIN on ' +
    'the backend must equal this page\'s origin character for character — no ' +
    'trailing slash, matching http/https, matching port.',

  'no-schema':
    'The API is connected to PostgreSQL, but the tables are missing. Run the ' +
    'three files in database/ against your Supabase project, or start the ' +
    'backend with AUTO_MIGRATE=true.',

  'no-database':
    'The API is up but cannot reach PostgreSQL. Check DATABASE_URL (or the ' +
    'DB_* variables) and that DB_SSLMODE is `require` for Supabase.',

  'no-cookie':
    'Sign-in succeeded but the session cookie was not stored. In production ' +
    'the API must run over HTTPS with ENV=production, so the cookie is sent ' +
    'as Secure + SameSite=None. A cookie cannot cross from https to http.',

  ok: 'Connected.',
};

async function check() {
  const origin = location.origin;
  const base = (window.ESM_API_URL || '').trim().replace(/\/+$/, '') || origin;

  const result = { origin, api: base, state: 'ok', detail: '' };

  // A page with no real origin cannot do cookie auth at all — this is the one
  // failure that no amount of backend configuration can fix.
  if (origin === 'null' || location.protocol === 'file:' || location.protocol === 'blob:') {
    result.state = 'no-origin';
    result.detail = DIAGNOSIS['no-origin'];
    return result;
  }

  let health;
  try {
    health = await api('/api/health');
  } catch (e) {
    // fetch cannot distinguish "server down" from "blocked by CORS", so probe
    // once more with a no-cors request: if that succeeds the server is up and
    // the block was CORS.
    let reachable = false;
    try {
      await fetch(base + '/api/health', { mode: 'no-cors', credentials: 'omit' });
      reachable = true;
    } catch (_) {}
    result.state = reachable ? 'cors' : 'unreachable';
    result.detail = DIAGNOSIS[result.state];
    return result;
  }

  if (!health || health.database === false) {
    result.state = 'no-database';
    result.detail = DIAGNOSIS['no-database'];
    return result;
  }
  if (health.schema === false) {
    result.state = 'no-schema';
    result.detail = DIAGNOSIS['no-schema'];
    return result;
  }

  result.health = health;
  result.detail = DIAGNOSIS.ok;

  // Is there a live session? A 401 here is normal before sign-in.
  try {
    result.user = await api('/api/auth/me');
    result.signedIn = true;
  } catch (e) {
    result.signedIn = false;
    if (e.status !== 401) result.detail = e.message;
  }
  return result;
}

/** Signs in and confirms the cookie actually stuck — the check that catches a
    Secure/SameSite mismatch, which otherwise looks like a wrong password. */
window.esmTestSignIn = async function (email, password) {
  const user = await Auth.signIn(email, password);
  const back = await Auth.restore();
  if (!back) {
    const err = new Error(DIAGNOSIS['no-cookie']);
    err.state = 'no-cookie';
    throw err;
  }
  console.info('[ESM] signed in as %s (%s)', back.name || back.email, back.role);
  return back;
};

window.esmCheckConnection = check;

check().then((r) => {
  window.ESM_CONNECTION = r;
  const line = '[ESM] ' + r.state.toUpperCase() + ' — page ' + r.origin + ' → api ' + r.api;
  if (r.state === 'ok') {
    console.info(line + (r.signedIn ? ' — signed in' : ' — not signed in'));
  } else {
    console.error(line + '\n' + r.detail);
  }
  window.dispatchEvent(new CustomEvent('esm:connection', { detail: r }));
}).catch((e) => {
  window.ESM_CONNECTION = { state: 'unreachable', detail: String(e && e.message) };
  console.error('[ESM] connection check failed', e);
}).finally(() => {
  // Release the app whatever happened — a failed check must not hang the UI.
  window.ESM_READY_DONE = true;
  if (window.__esmReadyResolve) window.__esmReadyResolve();
});

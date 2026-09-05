# Frontend

A static site. No build step and no bundler — `index.html` is the app, and the
files beside it are plain ES modules.

```bash
npx serve . -l 3000
```

## Why the previous file could never connect

The old `frontend/index.html` was a **self-contained bundle**: a small loader
that base64-unpacked the whole app and ran it from a `blob:` URL. That is
excellent for emailing someone a working file, and impossible for a
cookie-authenticated API, for three separate reasons:

1. **A blob document has no origin.** It reports `null`. `Access-Control-Allow-Origin`
   can never match `null`, and browsers refuse to store or send cookies for a
   null-origin page. So no session could ever be established, whatever the
   backend was configured with.
2. **`window.ESM_API_URL` was invisible to it.** The variable was set on the
   outer loader page; the app ran inside a different document.
3. **The bundled app never loaded `esm-auth.js` or `esm-db.js`.** Those two
   files sat in the folder unreferenced, so nothing called the API at all.

`index.html` is now the real app source served from a real origin, and it loads
the API layer in `<head>`. That is the part that was missing.

## Files

| File | Role |
| --- | --- |
| `index.html` | the application — UI, screens, workflows, all unchanged |
| `support.js` | the app's runtime, loaded first |
| `esm-connect.js` | publishes `window.Auth` / `window.DB`, runs the connection check |
| `esm-auth.js` | sign-in, session restore, user management |
| `esm-db.js` | every data read and write |
| `assets/` | brand mark and images |

## Pointing at the backend

One line, near the top of `index.html`:

```html
<script>window.ESM_API_URL = "https://your-api.up.railway.app";</script>
```

Empty means "same origin as this page". No keys live here — the backend holds
the database credentials; this is just an address.

## Checking the connection

Open the console. On every load you get one line:

```
[ESM] OK — page https://esm.vercel.app → api https://esm-api.up.railway.app — not signed in
```

Anything other than `OK` prints the specific cause. Then, to prove the cookie
round-trip actually works:

```js
await esmTestSignIn('aditya@asksummu.in', 'your-password')
```

That signs in and immediately calls `/api/auth/me` again. If the second call
comes back empty, the password was fine and the **cookie** was rejected — the
one failure that looks like a wrong password but is not.

`window.ESM_CONNECTION` holds the last result; `esmCheckConnection()` re-runs it.

## The four things that break it

| Console says | Fix |
| --- | --- |
| `NO-ORIGIN` | You opened the file directly. Serve it over http:// |
| `UNREACHABLE` | Wrong `ESM_API_URL`, or the backend is asleep or not deployed |
| `CORS` | `ALLOWED_ORIGIN` must equal this page's origin exactly — no trailing slash, matching scheme and port |
| `NO-SCHEMA` | Run the files in `database/`, or start the backend with `AUTO_MIGRATE=true` |

Two more worth knowing, because neither reports as an error:

- **Mixed scheme.** An `https://` page cannot receive a cookie from an
  `http://` API. Both must be HTTPS in production.
- **`ENV` not set to `production`.** The cookie is then issued as
  `SameSite=Lax`, which the browser will not send across origins. Set
  `ENV=production` whenever the frontend and API are on different hosts, even
  when testing.

## Still to do

`window.Auth` and `window.DB` are loaded and working, but the app's own logic
class still reads and writes `localStorage` internally — it was written that way
before there was a server. So the app runs, and the API is reachable and proven
by the check above, but the screens are not yet reading through it.

Swapping those call sites over is the remaining work: each one becomes the
matching `DB.*` call (`DB.components()`, `DB.createComponent(...)`,
`DB.restock(...)` and so on — `esm-db.js` covers every endpoint). Say the word
and I'll wire the screens through, starting with sign-in and the inventory list.

## Offline

Unchanged. Reads fall back to the localStorage cache, writes made while offline
are queued and replayed on the next successful call, and the server wins on
conflict. `DB.pending` is the queue depth; `DB.retry()` flushes it.

The AI assistant and voice commands are entirely local and unaffected.

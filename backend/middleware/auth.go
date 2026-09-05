// Package middleware holds the request pipeline: CORS, rate limiting,
// authentication and the admin gate.
//
// The security boundary is here, not in the frontend. Every protected handler
// runs behind Auth, which resolves the session cookie to a live profile row
// loaded fresh from Postgres on every request — so a deactivated account or a
// changed role takes effect immediately, without waiting for a token to expire.
package middleware

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/asksummu/electronic-store-manager/backend/db"
)

// CookieName is the only piece of session state the browser holds, and it is
// opaque: a random token, HttpOnly, never readable by JavaScript.
const CookieName = "esm_session"

type ctxKey int

const userKey ctxKey = 1

// User is the authenticated caller, as loaded from the database.
type User struct {
	ID        string          `json:"id"`
	Email     string          `json:"email"`
	Name      string          `json:"name"`
	Role      string          `json:"role"`
	Active    bool            `json:"active"`
	Perms     json.RawMessage `json:"perms,omitempty"`
	SessionID string          `json:"-"`
	ExpiresAt time.Time       `json:"-"`
}

func (u *User) IsAdmin() bool { return u != nil && u.Role == "admin" }

// Actor is the display name written into activity rows.
func (u *User) Actor() string {
	if u == nil {
		return "system"
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// FromContext returns the caller, if the request went through Auth.
func FromContext(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(userKey).(*User)
	return u, ok
}

// MustUser is for handlers mounted behind Auth, where absence is a bug.
func MustUser(r *http.Request) *User {
	u, _ := FromContext(r.Context())
	if u == nil {
		return &User{Name: "system", Role: "staff"}
	}
	return u
}

func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// ---------------------------------------------------------------------- Auth

// Auth requires a valid, unexpired session and attaches the profile.
func Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := resolve(r)
		if err != nil {
			unauthorized(w, err.Error())
			return
		}
		maybeRefresh(r.Context(), u)
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

// Optional attaches the profile when there is one but never rejects. Used by
// /api/health and /api/auth/me, which must answer for signed-out visitors too.
func Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, err := resolve(r); err == nil {
			r = r.WithContext(WithUser(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}

type authError string

func (e authError) Error() string { return string(e) }

// resolve reads the cookie, looks the session up, and loads the profile. The
// role always comes from the database — never from a header or a request body.
func resolve(r *http.Request) (*User, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, authError("Please sign in.")
	}

	var (
		u         User
		expires   time.Time
		sessionID string
	)
	err = db.GetDB().QueryRow(r.Context(), `
		select s.id, s.expires_at, p.id, p.email, p.name, p.role, p.active, p.perms
		  from public.sessions s
		  join public.profiles p on p.id = s.user_id
		 where s.token = $1`, c.Value).
		Scan(&sessionID, &expires, &u.ID, &u.Email, &u.Name, &u.Role, &u.Active, &u.Perms)
	if err != nil {
		return nil, authError("Your session has ended. Please sign in again.")
	}
	if time.Now().After(expires) {
		// Tidy up as we go rather than needing a cron job.
		_, _ = db.GetDB().Exec(r.Context(), `delete from public.sessions where id = $1`, sessionID)
		return nil, authError("Your session expired. Please sign in again.")
	}
	if !u.Active {
		return nil, authError("This account is deactivated. Ask an admin to reactivate it.")
	}
	u.SessionID = sessionID
	u.ExpiresAt = expires
	return &u, nil
}

// maybeRefresh slides the expiry forward when a session is within a day of
// ending, so an active user is never logged out mid-task.
func maybeRefresh(ctx context.Context, u *User) {
	if time.Until(u.ExpiresAt) > 24*time.Hour {
		return
	}
	_, _ = db.GetDB().Exec(ctx,
		`update public.sessions set expires_at = now() + interval '7 days' where id = $1`,
		u.SessionID)
}

// ---------------------------------------------------------------------- Admin

// Admin gates the endpoints that manage other people's access or the whole
// store: users, global settings, backup and restore, automation config,
// emptying the trash.
func Admin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := FromContext(r.Context())
		if !ok || u == nil {
			unauthorized(w, "Please sign in.")
			return
		}
		// Re-read the role from the database: a demotion applies at once.
		var role string
		var active bool
		if err := db.GetDB().QueryRow(r.Context(),
			`select role, active from public.profiles where id = $1`, u.ID).
			Scan(&role, &active); err != nil {
			forbidden(w, "Could not verify your access.")
			return
		}
		if !active {
			forbidden(w, "This account is deactivated.")
			return
		}
		if role != "admin" {
			forbidden(w, "That action needs an admin account.")
			return
		}
		u.Role = role
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter, msg string) { writeErr(w, 401, msg) }
func forbidden(w http.ResponseWriter, msg string)    { writeErr(w, 403, msg) }

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ------------------------------------------------------------------ plumbing

// MaxBody caps request bodies at 10MB — enough for a full store backup, small
// enough that a bad actor cannot exhaust memory.
func MaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// Recover turns a panic into a 500 with a safe message, and logs the detail
// server-side only.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic: %v — %s %s", p, r.Method, r.URL.Path)
				writeErr(w, 500, "Something went wrong. Please try again.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RequestLog logs method, path, status and duration. No bodies, no headers,
// so credentials cannot end up in logs.
func RequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, sw.status,
			time.Since(start).Round(time.Millisecond), ClientIP(r))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }

// ClientIP prefers the proxy header, since the backend runs behind one.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

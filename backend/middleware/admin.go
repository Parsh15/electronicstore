package middleware

import (
	"encoding/json"
	"net/http"
)

// AdminOnly is the handler-level equivalent of the Admin middleware, for the
// handful of places where one route in a group needs the stricter check.
func AdminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := FromContext(r.Context())
		if !ok || !u.IsAdmin() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "That action needs an admin account.",
			})
			return
		}
		h(w, r)
	}
}

// HasPerm reports whether the caller may act in a named section. Admins always
// may. Staff are allowed unless their perms object explicitly says otherwise —
// matching how the existing UI treats a missing permission as "allowed", so no
// current account loses access when this backend is deployed.
func HasPerm(u *User, section string) bool {
	if u == nil {
		return false
	}
	if u.IsAdmin() {
		return true
	}
	if !u.Active {
		return false
	}
	if len(u.Perms) == 0 {
		return true
	}
	var m map[string]any
	if err := json.Unmarshal(u.Perms, &m); err != nil {
		return true
	}
	v, present := m[section]
	if !present {
		return true
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != "none" && t != "hidden" && t != "false"
	default:
		return true
	}
}

// RequirePerm guards a route by section name for staff accounts.
func RequirePerm(section string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, _ := FromContext(r.Context())
			if !HasPerm(u, section) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "Your account does not have access to " + section + ".",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

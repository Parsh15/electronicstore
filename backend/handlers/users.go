package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
	"github.com/asksummu/electronic-store-manager/backend/models"
	"github.com/asksummu/electronic-store-manager/backend/services"
)

// User management. Every route in this file sits behind middleware.Admin, so
// role is verified against the database on each request — a demoted admin
// loses access immediately, not when their session expires.
type UserHandler struct{}

func (h *UserHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)

	r.Route("/{id}", func(ur chi.Router) {
		ur.Get("/", h.Get)
		ur.Put("/", h.Update)
		ur.Delete("/", h.Delete)
		ur.Put("/role", h.SetRole)
		ur.Put("/activate", h.setActive(true))
		ur.Put("/deactivate", h.setActive(false))
	})
}

// GET /api/users
func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	body, err := db.QueryJSON(r.Context(), db.GetDB(), `
		select `+models.ProfileColumns+`,
		       (select count(*) from public.sessions s
		         where s.user_id = public.profiles.id and s.expires_at > now()) as "activeSessions"
		  from public.profiles
		 order by created_at asc`)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /api/users/:id
func (h *UserHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	body, err := db.QueryJSONRow(r.Context(), db.GetDB(),
		`select `+models.ProfileColumns+` from public.profiles where id = $1`, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// POST /api/users
//
// This replaces the Supabase Edge Function the old build called. Password
// hashing happens here with bcrypt; the plaintext never touches the database
// and is never logged.
func (h *UserHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.CreateUserRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	hash, err := services.HashPassword(req.Password)
	if err != nil {
		fail(w, r, err)
		return
	}

	var body []byte
	err = db.InTx(r.Context(), func(tx pgx.Tx) error {
		var taken bool
		if err := tx.QueryRow(r.Context(),
			`select exists (select 1 from public.profiles where lower(email) = $1)`,
			req.Email).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return services.ErrEmailTaken
		}

		var id string
		if err := tx.QueryRow(r.Context(), `
			insert into public.profiles (email, name, password_hash, role, perms, created_by, active)
			values ($1, $2, $3, $4, $5, $6, true)
			returning id`,
			req.Email, req.Name, hash, req.Role, req.Perms, actor(r)).Scan(&id); err != nil {
			return err
		}
		if err := logActivity(r, tx, `Created account for `+req.Name,
			"＋", "#5faa87", "user", id); err != nil {
			return err
		}
		var err error
		body, err = db.QueryJSONRow(r.Context(), tx,
			`select `+models.ProfileColumns+` from public.profiles where id = $1`, id)
		return err
	})
	if err == services.ErrEmailTaken {
		plain(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusCreated, body)
}

// PUT /api/users/:id — a patch. Passing a password re-hashes it and signs that
// account out everywhere.
func (h *UserHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req models.UpdateUserRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	me := middleware.MustUser(r)
	// An admin must not be able to lock themselves out of their own store.
	if id == me.ID {
		if req.Role != nil && *req.Role != "admin" {
			bad(w, models.Invalid("role", "You cannot remove your own admin access."))
			return
		}
		if req.Active != nil && !*req.Active {
			bad(w, models.Invalid("active", "You cannot deactivate your own account."))
			return
		}
	}
	if req.Role != nil && *req.Role != "admin" {
		if last, err := isLastAdmin(r, id); err == nil && last {
			bad(w, models.Invalid("role", "This is the only admin account left."))
			return
		}
	}

	if req.Password != nil {
		if err := services.SetPasswordFor(r.Context(), id, *req.Password); err != nil {
			fail(w, r, err)
			return
		}
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(), `
		update public.profiles p set
		  name   = coalesce($2, p.name),
		  email  = coalesce(lower($3), p.email),
		  role   = coalesce($4, p.role),
		  active = coalesce($5, p.active),
		  perms  = coalesce($6, p.perms)
		 where p.id = $1
		returning `+models.ProfileColumns,
		id, req.Name, req.Email, req.Role, req.Active, req.Perms)
	if err != nil {
		fail(w, r, err)
		return
	}

	// A deactivated account's sessions are dropped at once, so the change takes
	// effect on their next request rather than at token expiry.
	if req.Active != nil && !*req.Active {
		_, _ = db.GetDB().Exec(r.Context(), `delete from public.sessions where user_id = $1`, id)
	}
	_ = logActivity(r, db.GetDB(), "Account updated", "✎", "#8da2c8", "user", id)
	writeRaw(w, http.StatusOK, body)
}

// DELETE /api/users/:id
func (h *UserHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	me := middleware.MustUser(r)
	if id == me.ID {
		bad(w, models.Invalid("", "You cannot delete your own account."))
		return
	}
	if last, err := isLastAdmin(r, id); err == nil && last {
		bad(w, models.Invalid("", "This is the only admin account left."))
		return
	}

	var name string
	err := db.InTx(r.Context(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(),
			`select name from public.profiles where id = $1`, id).Scan(&name); err != nil {
			return err
		}
		// Sessions cascade with the profile; activity rows keep the actor's
		// name so history survives the account.
		if _, err := tx.Exec(r.Context(), `delete from public.profiles where id = $1`, id); err != nil {
			return err
		}
		return logActivity(r, tx, "Deleted account "+name, "×", "#c0655f", "user", id)
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ok(w, map[string]bool{"deleted": true})
}

// PUT /api/users/:id/role
func (h *UserHandler) SetRole(w http.ResponseWriter, r *http.Request) {
	id, valid := idParam(w, r, "id")
	if !valid {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := models.OneOf("role", req.Role, "admin", "staff"); p != nil {
		bad(w, p)
		return
	}
	if id == middleware.MustUser(r).ID && req.Role != "admin" {
		bad(w, models.Invalid("role", "You cannot remove your own admin access."))
		return
	}
	if req.Role != "admin" {
		if last, err := isLastAdmin(r, id); err == nil && last {
			bad(w, models.Invalid("role", "This is the only admin account left."))
			return
		}
	}

	body, err := db.QueryJSONRow(r.Context(), db.GetDB(),
		`update public.profiles p set role = $2 where p.id = $1
		 returning `+models.ProfileColumns, id, req.Role)
	if err != nil {
		fail(w, r, err)
		return
	}
	_ = logActivity(r, db.GetDB(), "Role changed to "+req.Role, "⚿", "#8da2c8", "user", id)
	writeRaw(w, http.StatusOK, body)
}

// PUT /api/users/:id/activate and /deactivate
func (h *UserHandler) setActive(active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, valid := idParam(w, r, "id")
		if !valid {
			return
		}
		if !active {
			if id == middleware.MustUser(r).ID {
				bad(w, models.Invalid("", "You cannot deactivate your own account."))
				return
			}
			if last, err := isLastAdmin(r, id); err == nil && last {
				bad(w, models.Invalid("", "This is the only admin account left."))
				return
			}
		}

		body, err := db.QueryJSONRow(r.Context(), db.GetDB(),
			`update public.profiles p set active = $2 where p.id = $1
			 returning `+models.ProfileColumns, id, active)
		if err != nil {
			fail(w, r, err)
			return
		}
		if !active {
			_, _ = db.GetDB().Exec(r.Context(), `delete from public.sessions where user_id = $1`, id)
		}

		verb := "Reactivated"
		if !active {
			verb = "Deactivated"
		}
		_ = logActivity(r, db.GetDB(), verb+" account", "⚿", "#8da2c8", "user", id)
		writeRaw(w, http.StatusOK, body)
	}
}

// isLastAdmin guards every path that could leave the store with no admin.
func isLastAdmin(r *http.Request, id string) (bool, error) {
	var count int
	var role string
	if err := db.GetDB().QueryRow(r.Context(),
		`select role from public.profiles where id = $1`, id).Scan(&role); err != nil {
		return false, err
	}
	if role != "admin" {
		return false, nil
	}
	if err := db.GetDB().QueryRow(r.Context(),
		`select count(*) from public.profiles where role = 'admin' and active`).Scan(&count); err != nil {
		return false, err
	}
	return count <= 1, nil
}

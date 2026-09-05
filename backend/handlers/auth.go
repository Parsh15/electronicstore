package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
	"github.com/asksummu/electronic-store-manager/backend/models"
	"github.com/asksummu/electronic-store-manager/backend/services"
)

type AuthHandler struct {
	Auth *services.AuthService
}

func NewAuthHandler(a *services.AuthService) *AuthHandler { return &AuthHandler{Auth: a} }

// Routes: /api/auth/*
//
// signup, login and me are reachable without a session; the rest require one.
// The rate limiter is applied to this whole group by the router.
func (h *AuthHandler) Routes(r chi.Router) {
	r.Post("/signup", h.Signup)
	r.Post("/login", h.Login)
	r.Post("/logout", h.Logout)
	r.Get("/me", h.Me)

	r.Group(func(pr chi.Router) {
		pr.Use(middleware.Auth)
		pr.Post("/refresh", h.Refresh)
		pr.Post("/change-password", h.ChangePassword)
	})
}

// POST /api/auth/signup
func (h *AuthHandler) Signup(w http.ResponseWriter, r *http.Request) {
	var req models.SignupRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	profile, err := h.Auth.Signup(r.Context(), r, w, &req)
	if errors.Is(err, services.ErrEmailTaken) {
		plain(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusCreated, profile)
}

// POST /api/auth/login
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req models.LoginRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}

	profile, err := h.Auth.Login(r.Context(), r, w, &req)
	switch {
	case errors.Is(err, services.ErrBadCredentials):
		plain(w, http.StatusUnauthorized, err)
		return
	case errors.Is(err, services.ErrInactive):
		plain(w, http.StatusForbidden, err)
		return
	case err != nil:
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, profile)
}

// POST /api/auth/logout — always succeeds, so a stale cookie can always be
// cleared.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	_ = h.Auth.Logout(r.Context(), r, w)
	ok(w, map[string]bool{"signedOut": true})
}

// GET /api/auth/me
//
// This is the frontend's session-restore call: with a valid cookie it returns
// the profile, and without one a 401 the client treats as "show the sign-in
// screen" rather than an error.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	u, okUser := middleware.FromContext(r.Context())
	if !okUser || u == nil {
		errorJSON(w, http.StatusUnauthorized, "Not signed in.")
		return
	}
	profile, err := services.Me(r.Context(), u)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeRaw(w, http.StatusOK, profile)
}

// POST /api/auth/refresh
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r)
	if err := h.Auth.Refresh(r.Context(), r, w, u); err != nil {
		plain(w, http.StatusUnauthorized, err)
		return
	}
	ok(w, map[string]bool{"refreshed": true})
}

// POST /api/auth/change-password
func (h *AuthHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var req models.ChangePasswordRequest
	if err := decode(r, &req); err != nil {
		plain(w, http.StatusBadRequest, err)
		return
	}
	if p := req.Validate(); p != nil {
		bad(w, p)
		return
	}
	if req.CurrentPassword == req.NewPassword {
		bad(w, models.Invalid("newPassword", "Choose a password you have not used here before."))
		return
	}

	u := middleware.MustUser(r)
	if err := h.Auth.ChangePassword(r.Context(), r, u, &req); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			fail(w, r, err)
			return
		}
		// "Your current password is incorrect." is the expected case here.
		plain(w, http.StatusBadRequest, err)
		return
	}
	ok(w, map[string]bool{"changed": true})
}

// Package services holds the logic that is more than a query: password
// handling and sessions, the automation engine, backup/restore, and report
// assembly. Handlers stay thin — parse, validate, call a service, respond.
package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
	"github.com/asksummu/electronic-store-manager/backend/models"
)

// BcryptCost 12 — the specified factor. Roughly a quarter-second per hash on
// typical hosting hardware, which is the point: it makes guessing expensive.
const BcryptCost = 12

// ErrBadCredentials is deliberately returned for both a missing account and a
// wrong password, so the API cannot be used to discover which emails exist.
var (
	ErrBadCredentials = errors.New("Email or password is incorrect.")
	ErrInactive       = errors.New("This account is deactivated. Ask an admin to reactivate it.")
	ErrEmailTaken     = errors.New("An account with that email already exists.")
)

type AuthService struct {
	MaxAge     time.Duration
	Production bool
}

func NewAuthService(maxAge time.Duration, production bool) *AuthService {
	if maxAge <= 0 {
		maxAge = 7 * 24 * time.Hour
	}
	return &AuthService{MaxAge: maxAge, Production: production}
}

// ------------------------------------------------------------------ passwords

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("cannot hash password: %w", err)
	}
	return string(h), nil
}

func CheckPassword(hash, pw string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// --------------------------------------------------------------------- signup

// Signup creates an account and signs it in. The very first account in an
// empty store becomes the admin — otherwise a fresh deployment would have
// nobody able to invite anyone.
func (a *AuthService) Signup(ctx context.Context, r *http.Request, w http.ResponseWriter,
	req *models.SignupRequest) ([]byte, error) {

	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, err
	}

	var profile []byte
	err = db.InTx(ctx, func(tx pgx.Tx) error {
		var taken bool
		if err := tx.QueryRow(ctx,
			`select exists (select 1 from public.profiles where lower(email) = $1)`,
			req.Email).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return ErrEmailTaken
		}

		var count int
		if err := tx.QueryRow(ctx, `select count(*) from public.profiles`).Scan(&count); err != nil {
			return err
		}
		role := "staff"
		if count == 0 {
			role = "admin"
		}

		var id string
		if err := tx.QueryRow(ctx, `
			insert into public.profiles (email, name, password_hash, role, active, created_by)
			values ($1, $2, $3, $4, true, 'self')
			returning id`, req.Email, req.Name, hash, role).Scan(&id); err != nil {
			return err
		}

		token, err := a.newSession(ctx, tx, id, r)
		if err != nil {
			return err
		}
		a.setCookie(w, token, a.MaxAge)

		if _, err := tx.Exec(ctx, `
			insert into public.activity (body, glyph, color, actor, actor_id, entity_type, entity_id)
			values ($1, '＋', '#5faa87', $2, $3, 'user', $3)`,
			"Account created for "+req.Name, req.Name, id); err != nil {
			return err
		}

		profile, err = db.QueryJSONRow(ctx, tx,
			`select `+models.ProfileColumns+` from public.profiles where id = $1`, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// ---------------------------------------------------------------------- login

func (a *AuthService) Login(ctx context.Context, r *http.Request, w http.ResponseWriter,
	req *models.LoginRequest) ([]byte, error) {

	var id, hash string
	var active bool
	fmt.Println("LOGIN RECEIVED")
	err := db.GetDB().QueryRow(ctx,
		`select id, password_hash, active from public.profiles where lower(email) = $1`,
		req.Email).Scan(&id, &hash, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		// Spend comparable time on a miss so timing does not reveal whether
		// the address is registered.
		_, _ = bcrypt.GenerateFromPassword([]byte(req.Password), BcryptCost)
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if !CheckPassword(hash, req.Password) {
		return nil, ErrBadCredentials
	}
	if !active {
		return nil, ErrInactive
	}

	var profile []byte
	err = db.InTx(ctx, func(tx pgx.Tx) error {
		token, err := a.newSession(ctx, tx, id, r)
		if err != nil {
			return err
		}
		a.setCookie(w, token, a.MaxAge)

		if _, err := tx.Exec(ctx,
			`update public.profiles set last_login_at = now() where id = $1`, id); err != nil {
			return err
		}
		profile, err = db.QueryJSONRow(ctx, tx,
			`select `+models.ProfileColumns+` from public.profiles where id = $1`, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// --------------------------------------------------------------------- logout

func (a *AuthService) Logout(ctx context.Context, r *http.Request, w http.ResponseWriter) error {
	if c, err := r.Cookie(middleware.CookieName); err == nil && c.Value != "" {
		_, _ = db.GetDB().Exec(ctx, `delete from public.sessions where token = $1`, c.Value)
	}
	a.setCookie(w, "", -time.Hour) // expire it in the browser too
	return nil
}

// -------------------------------------------------------------------- refresh

// Refresh extends the current session and re-issues the cookie.
func (a *AuthService) Refresh(ctx context.Context, r *http.Request, w http.ResponseWriter,
	u *middleware.User) error {

	c, err := r.Cookie(middleware.CookieName)
	if err != nil {
		return errors.New("Please sign in.")
	}
	if _, err := db.GetDB().Exec(ctx,
		`update public.sessions set expires_at = now() + $2::interval where token = $1`,
		c.Value, fmt.Sprintf("%d seconds", int(a.MaxAge.Seconds()))); err != nil {
		return err
	}
	a.setCookie(w, c.Value, a.MaxAge)
	return nil
}

// ------------------------------------------------------------ password change

// ChangePassword verifies the current password, writes the new hash, and drops
// every other session for that account — so a password change also signs out
// any device the user was worried about.
func (a *AuthService) ChangePassword(ctx context.Context, r *http.Request,
	u *middleware.User, req *models.ChangePasswordRequest) error {

	var hash string
	if err := db.GetDB().QueryRow(ctx,
		`select password_hash from public.profiles where id = $1`, u.ID).Scan(&hash); err != nil {
		return err
	}
	if !CheckPassword(hash, req.CurrentPassword) {
		return errors.New("Your current password is incorrect.")
	}

	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		return err
	}

	keep := ""
	if c, err := r.Cookie(middleware.CookieName); err == nil {
		keep = c.Value
	}

	return db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`update public.profiles set password_hash = $2 where id = $1`, u.ID, newHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`delete from public.sessions where user_id = $1 and token <> $2`, u.ID, keep); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into public.activity (body, glyph, color, actor, actor_id, entity_type, entity_id)
			values ('Password changed', '⚿', '#8da2c8', $1, $2, 'user', $2)`, u.Actor(), u.ID)
		return err
	})
}

// SetPasswordFor is the admin path: no current-password check, and every
// session for that account is revoked.
func SetPasswordFor(ctx context.Context, userID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`update public.profiles set password_hash = $2 where id = $1`, userID, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return db.ErrNotFound
		}
		_, err = tx.Exec(ctx, `delete from public.sessions where user_id = $1`, userID)
		return err
	})
}

// -------------------------------------------------------------------- session

// newSession writes the session row and returns the opaque token. 32 bytes of
// crypto/rand — not a JWT, so revocation is a DELETE rather than a wait.
func (a *AuthService) newSession(ctx context.Context, q db.Querier, userID string, r *http.Request) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cannot generate a session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	ua := r.Header.Get("User-Agent")
	if len(ua) > 400 {
		ua = ua[:400]
	}

	if _, err := q.Exec(ctx, `
		insert into public.sessions (user_id, token, expires_at, user_agent, ip_address)
		values ($1, $2, now() + $3::interval, $4, $5)`,
		userID, token, fmt.Sprintf("%d seconds", int(a.MaxAge.Seconds())),
		ua, middleware.ClientIP(r)); err != nil {
		return "", err
	}

	// Opportunistic housekeeping; a failure here must not fail the sign-in.
	_, _ = q.Exec(ctx, `delete from public.sessions where expires_at < now()`)
	return token, nil
}

// setCookie writes the only cookie this app uses. HttpOnly means JavaScript
// cannot read it, which is why no token is ever placed in localStorage.
func (a *AuthService) setCookie(w http.ResponseWriter, token string, maxAge time.Duration) {
	c := &http.Cookie{
		Name:     middleware.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   int(maxAge.Seconds()),
	}
	if a.Production {
		// The frontend and API sit on different hosts in production, so the
		// cookie must be SameSite=None to be sent at all — and SameSite=None
		// requires Secure. Locally, Lax over http keeps development simple.
		c.Secure = true
		c.SameSite = http.SameSiteNoneMode
	} else {
		c.SameSite = http.SameSiteLaxMode
	}
	if maxAge < 0 {
		c.Expires = time.Unix(0, 0)
	}
	http.SetCookie(w, c)
}

// Me returns the caller's profile as JSON.
func Me(ctx context.Context, u *middleware.User) ([]byte, error) {
	return db.QueryJSONRow(ctx, db.GetDB(),
		`select `+models.ProfileColumns+` from public.profiles where id = $1`, u.ID)
}

package models

import "strings"

// Profile is what the API returns for an account. The password hash has no
// json tag at all, so it cannot leak through a response even by accident.
type Profile struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Active      bool   `json:"active"`
	Perms       any    `json:"perms,omitempty"`
	CreatedBy   string `json:"createdBy,omitempty"`
	LastLoginAt *int64 `json:"lastLoginAt,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
}

// ProfileColumns is the shared select list — no password_hash, ever.
const ProfileColumns = `id, email, name, role, active, perms, created_by,
	extract(epoch from last_login_at) * 1000 as last_login_at,
	extract(epoch from created_at) * 1000    as created_at`

type SignupRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (r *SignupRequest) Normalise() {
	r.Name = strings.TrimSpace(r.Name)
	r.Email = strings.ToLower(strings.TrimSpace(r.Email))
}

func (r *SignupRequest) Validate() *Problem {
	r.Normalise()
	return First(
		Required("name", r.Name), MaxLen("name", r.Name, 80),
		ValidEmail("email", r.Email),
		ValidatePassword(r.Password),
	)
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (r *LoginRequest) Validate() *Problem {
	r.Email = strings.ToLower(strings.TrimSpace(r.Email))
	return First(Required("email", r.Email), Required("password", r.Password))
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (r *ChangePasswordRequest) Validate() *Problem {
	return First(
		Required("currentPassword", r.CurrentPassword),
		ValidatePassword(r.NewPassword),
	)
}

// CreateUserRequest is the admin-side account creation payload. Role arrives
// here but is only trusted because the route sits behind the Admin gate.
type CreateUserRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
	Perms    any    `json:"perms"`
}

func (r *CreateUserRequest) Validate() *Problem {
	r.Name = strings.TrimSpace(r.Name)
	r.Email = strings.ToLower(strings.TrimSpace(r.Email))
	if r.Role == "" {
		r.Role = "staff"
	}
	return First(
		Required("name", r.Name), MaxLen("name", r.Name, 80),
		ValidEmail("email", r.Email),
		ValidatePassword(r.Password),
		OneOf("role", r.Role, "admin", "staff"),
	)
}

// UpdateUserRequest is a patch: only non-nil fields are written.
type UpdateUserRequest struct {
	Name     *string `json:"name"`
	Email    *string `json:"email"`
	Role     *string `json:"role"`
	Active   *bool   `json:"active"`
	Perms    any     `json:"perms"`
	Password *string `json:"password"`
}

func (r *UpdateUserRequest) Validate() *Problem {
	Trim(r.Name)
	Trim(r.Email)
	if r.Name != nil {
		if p := First(Required("name", *r.Name), MaxLen("name", *r.Name, 80)); p != nil {
			return p
		}
	}
	if r.Email != nil {
		*r.Email = strings.ToLower(*r.Email)
		if p := ValidEmail("email", *r.Email); p != nil {
			return p
		}
	}
	if r.Role != nil {
		if p := OneOf("role", *r.Role, "admin", "staff"); p != nil {
			return p
		}
	}
	if r.Password != nil {
		if p := ValidatePassword(*r.Password); p != nil {
			return p
		}
	}
	return nil
}

// ValidatePassword is the single place the password policy lives.
func ValidatePassword(pw string) *Problem {
	if len(pw) < 8 {
		return Invalid("password", "Use at least 8 characters.")
	}
	if len(pw) > 128 {
		return Invalid("password", "That password is too long.")
	}
	if strings.TrimSpace(pw) == "" {
		return Invalid("password", "A password cannot be only spaces.")
	}
	return nil
}

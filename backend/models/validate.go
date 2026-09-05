// Package models holds the request shapes the API accepts and the validation
// each one enforces before any SQL runs.
//
// Validation returns a *Problem carrying the field name and a message written
// for the person using the app, which handlers render as a 400. Nothing here
// touches the database.
package models

import (
	"fmt"
	"regexp"
	"strings"
)

type Problem struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"error"`
}

func (p *Problem) Error() string { return p.Message }

func Invalid(field, msg string) *Problem { return &Problem{Field: field, Message: msg} }

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]+$`)
)

// IsUUID is used by handlers on every path parameter, so a malformed id is a
// clean 400 rather than a database type error.
func IsUUID(s string) bool { return uuidRe.MatchString(s) }

func ValidUUID(field, v string) *Problem {
	if !IsUUID(v) {
		return Invalid(field, "That record id is not valid.")
	}
	return nil
}

func Required(field, v string) *Problem {
	if strings.TrimSpace(v) == "" {
		return Invalid(field, Label(field)+" is required.")
	}
	return nil
}

func MaxLen(field, v string, n int) *Problem {
	if len([]rune(v)) > n {
		return Invalid(field, fmt.Sprintf("%s must be %d characters or fewer.", Label(field), n))
	}
	return nil
}

func InRange(field string, v, lo, hi int) *Problem {
	if v < lo || v > hi {
		return Invalid(field, fmt.Sprintf("%s must be between %d and %d.", Label(field), lo, hi))
	}
	return nil
}

func OneOf(field, v string, allowed ...string) *Problem {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return Invalid(field, Label(field)+" must be one of: "+strings.Join(allowed, ", ")+".")
}

func ValidEmail(field, v string) *Problem {
	if !emailRe.MatchString(strings.TrimSpace(v)) {
		return Invalid(field, "That does not look like an email address.")
	}
	return MaxLen(field, v, 200)
}

// Label turns a JSON field name into something readable in a message.
func Label(field string) string {
	switch field {
	case "minStock":
		return "Minimum stock"
	case "unitTracked":
		return "Unit tracking"
	case "reorderPoint":
		return "Reorder point"
	case "componentId":
		return "Component"
	case "projectId":
		return "Project"
	case "boxId":
		return "Box"
	case "eventId":
		return "Event"
	case "":
		return "That value"
	}
	out := []rune(field)
	out[0] = []rune(strings.ToUpper(string(out[0])))[0]
	s := string(out)
	s = regexp.MustCompile(`([a-z])([A-Z])`).ReplaceAllString(s, "$1 $2")
	return s
}

// First returns the first non-nil problem, so a Validate method reads as a
// straight list of checks.
func First(ps ...*Problem) *Problem {
	for _, p := range ps {
		if p != nil {
			return p
		}
	}
	return nil
}

// Trim normalises a pointer-to-string field in a PATCH-style payload.
func Trim(p *string) {
	if p != nil {
		*p = strings.TrimSpace(*p)
	}
}

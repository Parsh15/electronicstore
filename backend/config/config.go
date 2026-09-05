// Package config loads and validates every environment variable the backend
// needs. Nothing else in the program reads os.Getenv, so there is exactly one
// place to look when a deployment misbehaves.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// Database
	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string
	DBSSLMode  string
	DBURL      string // wins over the fields above when set

	// HTTP
	Port string
	Env  string

	// Sessions
	SessionSecret string
	SessionMaxAge time.Duration

	// CORS
	AllowedOrigins []string

	AutoMigrate bool
}

func (c *Config) IsProduction() bool { return strings.EqualFold(c.Env, "production") }

// DSN returns the pgx connection string.
func (c *Config) DSN() string {
	if c.DBURL != "" {
		return c.DBURL
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(c.DBUser), url.QueryEscape(c.DBPassword),
		c.DBHost, c.DBPort, c.DBName, c.DBSSLMode)
}

// Redacted is safe to log on boot.
func (c *Config) Redacted() string {
	host := c.DBHost
	if c.DBURL != "" {
		if u, err := url.Parse(c.DBURL); err == nil {
			host = u.Host
		}
	}
	return fmt.Sprintf("env=%s port=%s db=%s@%s ssl=%s origins=%s",
		c.Env, c.Port, c.DBName, host, c.DBSSLMode, strings.Join(c.AllowedOrigins, ","))
}

// Load reads .env when present (never required — hosted deployments inject real
// environment variables), then validates.
func Load() (*Config, error) {
	_ = godotenv.Load()

	c := &Config{
		DBHost:        env("DB_HOST", ""),
		DBPort:        envInt("DB_PORT", 5432),
		DBName:        env("DB_NAME", "postgres"),
		DBUser:        env("DB_USER", ""),
		DBPassword:    env("DB_PASSWORD", ""),
		DBSSLMode:     env("DB_SSLMODE", "require"),
		DBURL:         env("DATABASE_URL", ""),
		Port:          env("PORT", "8080"),
		Env:           env("ENV", "development"),
		SessionSecret: env("SESSION_SECRET", ""),
		SessionMaxAge: time.Duration(envInt("SESSION_MAX_AGE", 604800)) * time.Second,
		AutoMigrate:   env("AUTO_MIGRATE", "true") == "true",
	}

	for _, o := range strings.Split(env("ALLOWED_ORIGIN", ""), ",") {
		if o = strings.TrimSpace(strings.TrimRight(o, "/")); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}

	var missing []string
	if c.DBURL == "" {
		if c.DBHost == "" {
			missing = append(missing, "DB_HOST (or DATABASE_URL)")
		}
		if c.DBUser == "" {
			missing = append(missing, "DB_USER")
		}
		if c.DBPassword == "" {
			missing = append(missing, "DB_PASSWORD")
		}
	}
	if len(c.SessionSecret) < 32 {
		missing = append(missing, "SESSION_SECRET (at least 32 characters)")
	}
	if len(c.AllowedOrigins) == 0 {
		missing = append(missing, "ALLOWED_ORIGIN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing or invalid configuration: %s", strings.Join(missing, ", "))
	}

	// A wildcard origin cannot be combined with cookie credentials, so refuse it
	// outright rather than failing mysteriously in the browser.
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			return nil, fmt.Errorf("ALLOWED_ORIGIN cannot be * for a cookie-authenticated API")
		}
	}
	return c, nil
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(env(k, "")); err == nil {
		return v
	}
	return def
}

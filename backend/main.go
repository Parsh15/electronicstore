// Electronic Store Manager — API server.
//
//	Browser  →  this Go service  →  PostgreSQL (Supabase)
//
// The browser holds no database credentials and never speaks to Postgres. Every
// read and write arrives here as an HTTP request carrying an HttpOnly session
// cookie, is authorised against a profile row loaded from the database, and is
// executed with bound parameters.
//
// Run:      go run .
// Admin:    go run . -create-admin
// Migrate:  go run . -migrate-only
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"golang.org/x/term"

	"github.com/asksummu/electronic-store-manager/backend/config"
	"github.com/asksummu/electronic-store-manager/backend/db"
	"github.com/asksummu/electronic-store-manager/backend/handlers"
	"github.com/asksummu/electronic-store-manager/backend/middleware"
	"github.com/asksummu/electronic-store-manager/backend/models"
	"github.com/asksummu/electronic-store-manager/backend/services"
)

const version = "4.0.0"

func main() {
	var (
		createAdmin = flag.Bool("create-admin", false, "create an admin account and exit")
		migrateOnly = flag.Bool("migrate-only", false, "apply migrations and exit")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := db.Connect(ctx, cfg.DSN()); err != nil {
		cancel()
		log.Fatalf("database: %v", err)
	}
	cancel()
	defer db.Close()

	if cfg.AutoMigrate || *migrateOnly {
		mctx, mcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := db.Migrate(mctx); err != nil {
			mcancel()
			log.Fatalf("migrate: %v", err)
		}
		mcancel()
	}
	if *migrateOnly {
		log.Println("migrations complete")
		return
	}
	if *createAdmin {
		if err := promptCreateAdmin(); err != nil {
			log.Fatalf("create-admin: %v", err)
		}
		return
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second, // report PDFs take longer than a page load
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		log.Printf("Electronic Store Manager API v%s listening on :%s (%s)",
			version, cfg.Port, cfg.Redacted())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	// Graceful shutdown: stop accepting, let in-flight requests finish, then
	// close the pool. A restart mid-transaction rolls back rather than tears.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")

	sctx, scancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer scancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// router wires the whole API. Read it top to bottom for the shape of the
// service: global middleware, then the open endpoints, then everything behind
// a session, then the admin-only group.
func router(cfg *config.Config) http.Handler {
	r := chi.NewRouter()

	authLimit := middleware.AuthLimiter()
	apiLimit := middleware.APILimiter()

	r.Use(chimw.RealIP)
	r.Use(middleware.Recover)
	r.Use(middleware.RequestLog)
	r.Use(middleware.HTTPSRedirect(cfg.IsProduction()))
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.CORS(cfg.AllowedOrigins))
	r.Use(middleware.MaxBody(10 << 20)) // 10MB
	r.Use(chimw.Timeout(110 * time.Second))

	health := &handlers.HealthHandler{Version: version, Started: time.Now()}
	authSvc := services.NewAuthService(cfg.SessionMaxAge, cfg.IsProduction())
	authH := handlers.NewAuthHandler(authSvc)

	r.Route("/api", func(api chi.Router) {
		// ---- open --------------------------------------------------------
		api.With(middleware.Optional).Get("/health", health.Health)

		api.Route("/auth", func(a chi.Router) {
			a.Use(authLimit.Middleware)
			a.Use(middleware.Optional) // /me answers for signed-out callers too
			authH.Routes(a)
		})

		// ---- signed in ---------------------------------------------------
		api.Group(func(p chi.Router) {
			p.Use(apiLimit.Middleware)
			p.Use(middleware.Auth)

			p.Route("/components", (&handlers.ComponentHandler{}).Routes)
			p.Route("/units", (&handlers.UnitHandler{}).Routes)
			p.Route("/projects", (&handlers.ProjectHandler{}).Routes)
			p.Route("/suppliers", (&handlers.SupplierHandler{}).Routes)
			p.Route("/boxes", (&handlers.BoxHandler{}).Routes)
			p.Route("/labels", (&handlers.LabelHandler{}).Routes)
			p.Route("/events", (&handlers.EventHandler{}).Routes)
			p.Route("/funds", (&handlers.FundHandler{}).Routes)
			p.Route("/reports", (&handlers.ReportHandler{}).Routes)
			p.Route("/activity", (&handlers.ActivityHandler{}).Routes)
			p.Route("/voice", (&handlers.VoiceHandler{}).Routes)
			p.Route("/trash", (&handlers.TrashHandler{}).Routes)
			p.Route("/automation", (&handlers.AutomationHandler{}).Routes)

			// settings: reads open to members, writes gated inside
			p.Route("/settings", (&handlers.SettingsHandler{}).Routes)

			p.Get("/search", (&handlers.SearchHandler{}).Search)

			// ---- admin only ----------------------------------------------
			p.Group(func(adm chi.Router) {
				adm.Use(middleware.Admin)
				adm.Route("/users", (&handlers.UserHandler{}).Routes)
				adm.Route("/backup", (&handlers.BackupHandler{}).Routes)
			})
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"No such endpoint."}`, http.StatusNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"That method is not allowed here."}`, http.StatusMethodNotAllowed)
	})

	return r
}

// promptCreateAdmin is the one-time bootstrap: an interactive account creation
// so the first admin password is never written into a SQL file or a shell
// history entry.
func promptCreateAdmin() error {
	ctx := context.Background()
	in := bufio.NewReader(os.Stdin)

	fmt.Print("Name:  ")
	name, _ := in.ReadString('\n')
	fmt.Print("Email: ")
	email, _ := in.ReadString('\n')

	name = strings.TrimSpace(name)
	email = strings.ToLower(strings.TrimSpace(email))

	fmt.Print("Password (hidden): ")
	pwBytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("could not read the password: %w", err)
	}
	password := string(pwBytes)

	req := models.CreateUserRequest{Name: name, Email: email, Password: password, Role: "admin"}
	if p := req.Validate(); p != nil {
		return errors.New(p.Message)
	}

	hash, err := services.HashPassword(req.Password)
	if err != nil {
		return err
	}

	var id string
	err = db.GetDB().QueryRow(ctx, `
		insert into public.profiles (email, name, password_hash, role, active, created_by)
		values ($1, $2, $3, 'admin', true, 'cli')
		on conflict (email) do update
		  set password_hash = excluded.password_hash,
		      role = 'admin', active = true, name = excluded.name
		returning id`, req.Email, req.Name, hash).Scan(&id)
	if err != nil {
		return err
	}

	fmt.Printf("Admin account ready: %s (%s)\n", req.Name, req.Email)
	return nil
}

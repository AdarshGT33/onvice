package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AdarshGT33/onvice/internal/api"
	"github.com/AdarshGT33/onvice/internal/auth"
	"github.com/AdarshGT33/onvice/internal/builder"
	"github.com/AdarshGT33/onvice/internal/queue"
	"github.com/AdarshGT33/onvice/internal/router"
	"github.com/AdarshGT33/onvice/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Cancelled on Ctrl+C or SIGTERM. Created first, so a Ctrl+C
	// during startup also shuts down cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Load .env for local development. On a real server the variables
	// are set in the environment, so a missing file is not an error.
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("loading .env: %w", err)
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is not set")
	}

	// One connection pool for the whole program.
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("creating database pool: %w", err)
	}
	defer pool.Close()

	// Fail at startup if the database is unreachable.
	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	err = pool.Ping(pingCtx)
	cancelPing()
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	log.Println("Connected to PostgreSQL")

	// Wire the layers together: pool -> store -> handlers.
	db := store.New(pool)

	routerAddr := envOr("ROUTER_ADDR", ":8000")
	appsDomain := envOr("APPS_DOMAIN", "localhost")
	_, routerPort, err := net.SplitHostPort(routerAddr)
	if err != nil {
		return fmt.Errorf("ROUTER_ADDR %q: %w", routerAddr, err)
	}
	// NEW: TLS. Set both TLS_CERT_FILE and TLS_KEY_FILE and the API and the
	// app router serve HTTPS only; API keys and app traffic are encrypted.
	// The certificate must cover the API's host and *.APPS_DOMAIN.
	// Unset: plain HTTP, for local development.
	certFile, keyFile := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	if (certFile == "") != (keyFile == "") {
		// Half-configured TLS must not quietly fall back to plain HTTP.
		return errors.New("set both TLS_CERT_FILE and TLS_KEY_FILE, or neither")
	}
	var tlsConfig *tls.Config
	scheme := "http"
	if certFile != "" {
		certs, err := newCertReloader(certFile, keyFile)
		if err != nil {
			return err
		}
		// TLS 1.2 floor: older versions have known weaknesses, and every
		// current browser and HTTP client supports 1.2+.
		tlsConfig = &tls.Config{GetCertificate: certs.getCertificate, MinVersion: tls.VersionTLS12}
		scheme = "https"
	} else {
		log.Println("WARNING: TLS is off; API keys travel unencrypted. Set TLS_CERT_FILE and TLS_KEY_FILE outside local development.")
	}

	// NEW: the public URL users are shown. By default it is what the
	// router serves; behind a load balancer or another domain layout, set
	// APP_URL_TEMPLATE, e.g. https://{name}.apps.example.com
	appURL := envOr("APP_URL_TEMPLATE", defaultAppURL(scheme, appsDomain, routerPort))
	if !strings.Contains(appURL, "{name}") {
		// Fail at startup, not by showing every user the same wrong URL.
		return fmt.Errorf("APP_URL_TEMPLATE %q must contain {name}", appURL)
	}
	h := api.NewHandler(db, appURL)

	r := gin.Default()
	// NEW: every API route needs an API key. On the whole group, not per
	// route, so a route added later can't be left open by accident.
	// The router (app traffic, :8000) stays public: apps are websites.
	v1 := r.Group("/api/v1", auth.Middleware(db))
	v1.GET("/projects", h.ListProjects)
	v1.POST("/projects", h.TakingInputofRepoandProjectName)
	v1.POST("/projects/:name/deployments", h.Redeploy)
	v1.POST("/projects/:name/rollback", h.Rollback)
	v1.GET("/deployments/:id", h.GetDeployment)
	v1.GET("/deployments/:id/logs", h.DeploymentLogs)
	v1.GET("/projects/:name/logs", h.AppLogs)

	server := &http.Server{
		Addr:    ":8080",
		Handler: r,
		// Stops slow clients from holding connections open forever.
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConfig, // nil = plain HTTP
	}

	// NEW: app traffic gets its own listener, separate from the platform API,
	// so a visitor to someone's app can never reach /api/v1, and each side
	// can later get its own TLS, limits and firewall rules.
	routerServer := &http.Server{
		Addr:              routerAddr,
		Handler:           router.New(db, appsDomain),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConfig,
	}
	servers := []*http.Server{server, routerServer}

	// NEW: optional plain-HTTP listener (normally ":80") that only
	// redirects app visitors to https. Browsers try http:// first when
	// someone types an address; without this they'd get nothing.
	if addr := os.Getenv("HTTP_REDIRECT_ADDR"); addr != "" {
		if tlsConfig == nil {
			return errors.New("HTTP_REDIRECT_ADDR needs TLS: there is no https to redirect to")
		}
		servers = append(servers, &http.Server{
			Addr:              addr,
			Handler:           httpsRedirect(appsDomain, routerPort),
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	buildService := builder.New(db, "")

	worker := queue.NewWorker(db, map[string]queue.Handler{
		"build":  buildService.Handle,
		"deploy": buildService.Deploy,
	})

	var workerWG sync.WaitGroup
	workerWG.Add(1)

	go func() {
		defer workerWG.Done()
		worker.Run(ctx)
	}()

	serverErrors := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if s.TLSConfig != nil {
				log.Printf("Listening on %s (https)", s.Addr)
				// Empty file names: the certificate comes from
				// TLSConfig.GetCertificate, so renewals are picked up.
				serverErrors <- s.ListenAndServeTLS("", "")
				return
			}
			log.Printf("Listening on %s (http)", s.Addr)
			serverErrors <- s.ListenAndServe()
		}()
	}

	var runErr error

	select {
	case <-ctx.Done():
		log.Println("Shutdown signal received")

	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("server: %w", err)
		}
	}

	stop()

	// Give in-flight requests up to 10 seconds to finish.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)

	// Shutdown never stops app containers: apps keep running while the
	// platform restarts; only routing pauses.
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			runErr = errors.Join(
				runErr,
				fmt.Errorf("shutting down HTTP server %s: %w", s.Addr, err),
			)
			// Force-close active HTTP connections if graceful shutdown fails.
			if closeErr := s.Close(); closeErr != nil {
				runErr = errors.Join(runErr, closeErr)
			}
		}
	}

	cancelShutdown()

	workerWG.Wait()

	return runErr
}

// envOr returns the environment variable, or def when it is unset.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

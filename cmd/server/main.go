package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/AdarshGT33/onvice/internal/api"
	"github.com/AdarshGT33/onvice/internal/builder"
	"github.com/AdarshGT33/onvice/internal/queue"
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
	h := api.NewHandler(db)

	r := gin.Default()
	v1 := r.Group("/api/v1")
	v1.POST("/projects", h.TakingInputofRepoandProjectName)
	v1.GET("/deployments/:id", h.GetDeployment)

	server := &http.Server{
		Addr:    ":8080",
		Handler: r,
		// Stops slow clients from holding connections open forever.
		ReadHeaderTimeout: 10 * time.Second,
	}

	buildService := builder.New(db, "")

	worker := queue.NewWorker(db, map[string]queue.Handler{
		"build": buildService.Handle,
	})

	var workerWG sync.WaitGroup
	workerWG.Add(1)

	go func() {
		defer workerWG.Done()
		worker.Run(ctx)
	}()

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("Listening on %s", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

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

	if err := server.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(
			runErr,
			fmt.Errorf("shutting down HTTP server: %w", err),
		)
		// Force-close active HTTP connections if graceful shutdown fails.
		if closeErr := server.Close(); closeErr != nil {
			runErr = errors.Join(runErr, closeErr)
		}
	}

	cancelShutdown()

	workerWG.Wait()

	return runErr
}

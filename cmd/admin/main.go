// Command admin is the operator's tool. Usage:
//
//	go run ./cmd/admin issue-key <email>
//
// Creates the user if new and prints a fresh API key. For an existing user
// it replaces their key (use it when a key is lost or leaked); the old key
// stops working immediately. The key is printed once and never stored.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"time"

	"github.com/AdarshGT33/onvice/internal/auth"
	"github.com/AdarshGT33/onvice/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "issue-key" {
		fmt.Fprintln(os.Stderr, "usage: admin issue-key <email>")
		os.Exit(2)
	}
	// Lowercased: emails are case-insensitive in practice, and the
	// UNIQUE constraint should treat Alice@x and alice@x as one user.
	email := strings.ToLower(strings.TrimSpace(os.Args[2]))
	if !strings.Contains(email, "@") {
		log.Fatalf("%q doesn't look like an email address", email)
	}

	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("loading .env: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	token, hash, err := auth.NewToken()
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}
	if err := store.New(pool).IssueToken(ctx, email, hash); err != nil {
		log.Fatal(err)
	}

	fmt.Fprintf(os.Stderr, "API key for %s (shown once, store it now):\n", email)
	fmt.Println(token) // stdout only the key, so scripts can capture it
}

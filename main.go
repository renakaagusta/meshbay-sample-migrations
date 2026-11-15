package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// releaseTag is the release marker of this branch. /health reports it, so a
// rollback to an older release is visible without reading the deploy log.
const releaseTag = "v3-broken"

func main() {
	if runError := run(os.Args[1:]); runError != nil {
		log.Fatalf("%v", runError)
	}
}

func run(arguments []string) error {
	command := "serve"
	if len(arguments) > 0 {
		command = strings.TrimSpace(arguments[0])
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	runContext := context.Background()
	database, databaseError := pgxpool.New(runContext, databaseURL)
	if databaseError != nil {
		return fmt.Errorf("configure database: %w", databaseError)
	}
	defer database.Close()
	switch command {
	case "migrate":
		return applyMigrations(runContext, database)
	case "serve":
		return serve(runContext, database)
	case "version":
		log.Printf("release %s", releaseTag)
		return nil
	default:
		return fmt.Errorf("unknown command %q, use serve, migrate or version", command)
	}
}

func serve(runContext context.Context, database *pgxpool.Pool) error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	pingContext, cancelPing := context.WithTimeout(runContext, 5*time.Second)
	pingError := database.Ping(pingContext)
	cancelPing()
	if pingError != nil {
		// A missing database must not stop the release: the old revision keeps
		// serving while this one reports 503 from /health.
		log.Printf("database ping: %v", pingError)
	}
	service := &application{database: database}
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           newRouter(service),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on port %s (release %s)", port, releaseTag)
	return server.ListenAndServe()
}

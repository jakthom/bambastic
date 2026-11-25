// Command bambastic serves the operator application or connects an MCP stdio client.
package main

import (
	"bambastic/internal/backup"
	"bambastic/internal/bridge"
	"bambastic/internal/httpx"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func defaultDatabaseURL() string {
	if value := os.Getenv("BAMBASTIC_DATABASE_URL"); value != "" {
		return value
	}
	value := os.Getenv("DATABASE_URL")
	if strings.HasPrefix(value, "postgres://") || strings.HasPrefix(value, "postgresql://") {
		return value
	}
	return ""
}
func run(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "coverage" {
		return coverage(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "restore" {
		flags := flag.NewFlagSet("bambastic restore", flag.ContinueOnError)
		dir := flags.String("data", "", "New destination directory")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		if flags.NArg() != 1 {
			return fmt.Errorf("usage: bambastic restore --data NEW_DIRECTORY BACKUP.zip")
		}
		return backup.Restore(ctx, flags.Arg(0), *dir)
	}
	if len(args) > 0 && args[0] == "mcp" {
		flags := flag.NewFlagSet("bambastic mcp", flag.ContinueOnError)
		_ = flags.Bool("stdio", true, "Use MCP standard input and output")
		addr := flags.String("server", env("BAMBASTIC_URL", httpx.DefaultURL), "Running Bambastic server")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		c, e := bridge.New()
		if e != nil {
			return e
		}
		var diagnosticsRoot string
		if developmentBuild {
			diagnosticsRoot = env("BAMBASTIC_DATA_DIR", "data")
		}
		return c.Stdio(ctx, *addr, os.Getenv("BAMBASTIC_API_KEY"), diagnosticsRoot)
	}
	if len(args) > 0 && args[0] == "version" {
		fmt.Println("Bambastic 0.1.0")
		return nil
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	flags := flag.NewFlagSet("bambastic serve", flag.ContinueOnError)
	addr := flags.String("addr", env("BAMBASTIC_ADDR", httpx.DefaultListenAddr), "HTTP listen address")
	dir := flags.String("data", env("BAMBASTIC_DATA_DIR", "data"), "Installation data directory")
	databaseURL := defaultDatabaseURL()
	flags.Func("database-url", "PostgreSQL connection URL (defaults to the database environment setting or local SQLite)", func(value string) error { databaseURL = value; return nil })
	postgresSchema := flags.String("postgres-schema", env("BAMBASTIC_POSTGRES_SCHEMA", "bambastic"), "Application-specific PostgreSQL schema")
	if e := flags.Parse(args); e != nil {
		return e
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unknown command %q", flags.Arg(0))
	}
	for ctx.Err() == nil {
		app, e := createConfiguredApp(ctx, *dir, databaseOptions{url: databaseURL, schema: *postgresSchema})
		if e != nil {
			return e
		}
		log.Printf("Bambastic listening on %s", *addr)
		e = app.serve(ctx, *addr)
		app.close()
		if e != nil {
			return e
		}
	}
	return nil
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := run(ctx, os.Args[1:]); e != nil && !errors.Is(e, flag.ErrHelp) {
		log.Print(e)
		os.Exit(1)
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	serviceapp "readiness/service/internal/app"
	"readiness/service/internal/config"
	"readiness/service/internal/filing"
	"readiness/service/internal/irsclient"
	"readiness/service/internal/web"

	"readiness.local/postgres/lifecycle"
	postgresstore "readiness.local/postgres/store"
)

type firmList []string

func (list *firmList) String() string { return strings.Join(*list, ",") }
func (list *firmList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("firm must not be blank")
	}
	for _, existing := range *list {
		if existing == value {
			return fmt.Errorf("duplicate firm %s", value)
		}
	}
	*list = append(*list, value)
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: readiness <import|determine|plan|worker|serve>")
	}
	loaded, err := config.Load()
	if err != nil {
		return err
	}
	database, err := postgresstore.Open(ctx, loaded.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open lifecycle: %w", err)
	}
	defer database.Close()
	application := serviceapp.New(database)

	switch arguments[0] {
	case "import":
		flags := flag.NewFlagSet("import", flag.ContinueOnError)
		firmID := flags.String("firm", "", "firm ID")
		taxYear := flags.Int("tax-year", 2025, "tax year")
		input := flags.String("input", "", "CSV.GZ file or directory")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *firmID == "" || *input == "" || *taxYear != 2025 {
			return errors.New("import requires --firm, --tax-year 2025, and --input")
		}
		result, err := application.Import(ctx, *firmID, *taxYear, *input)
		return output(result, err)
	case "determine":
		flags := flag.NewFlagSet("determine", flag.ContinueOnError)
		firmID := flags.String("firm", "", "firm ID")
		datasetID := flags.Int64("dataset", 0, "dataset ID")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *firmID == "" || *datasetID <= 0 {
			return errors.New("determine requires --firm and --dataset")
		}
		result, err := application.Determine(ctx, *firmID, *datasetID)
		return output(result, err)
	case "plan":
		flags := flag.NewFlagSet("plan", flag.ContinueOnError)
		firmID := flags.String("firm", "", "firm ID")
		determinationID := flags.Int64("determination", 0, "determination ID")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *firmID == "" || *determinationID <= 0 {
			return errors.New("plan requires --firm and --determination")
		}
		result, err := application.Plan(ctx, *firmID, *determinationID)
		return output(result, err)
	case "worker":
		flags := flag.NewFlagSet("worker", flag.ContinueOnError)
		var firms firmList
		flags.Var(&firms, "firm", "firm ID; repeat for multiple firms")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if len(firms) == 0 {
			return errors.New("worker requires at least one --firm")
		}
		worker, err := makeWorker(loaded, database)
		if err != nil {
			return err
		}
		return worker.Run(ctx, firms)
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		var firms firmList
		flags.Var(&firms, "firm", "firm ID; repeat for multiple firms")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if len(firms) == 0 {
			return errors.New("serve requires at least one --firm")
		}
		worker, err := makeWorker(loaded, database)
		if err != nil {
			return err
		}
		controller := web.NewWorkerController(worker, firms)
		server, err := web.New(application, firms, controller)
		if err != nil {
			return err
		}
		slog.Info("web server starting", "address", loaded.HTTPAddr)
		err = web.ListenAndServe(ctx, loaded.HTTPAddr, server.Handler(), loaded.ShutdownTimeout)
		controller.Stop()
		return err
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func makeWorker(loaded config.Config, database lifecycle.DataLifecycle) (*filing.Worker, error) {
	client, err := irsclient.New(irsclient.Config{
		BaseURL: loaded.IRSBaseURL, BearerToken: loaded.IRSBearerToken,
		ConnectTimeout: loaded.ConnectTimeout, ResponseHeaderTimeout: loaded.ResponseHeaderTimeout, TotalTimeout: loaded.TotalTimeout,
	})
	if err != nil {
		return nil, err
	}
	return filing.NewWorker(database, client, fmt.Sprintf("worker-%d", os.Getpid()), loaded.WorkerLeaseDuration, loaded.WorkerIdleDelay, filing.Backoff{
		Initial: time.Second, Maximum: 5 * time.Minute, MaxAttempts: loaded.MaxAttempts,
	})
}

func output(value any, err error) error {
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

var _ lifecycle.DataLifecycle = (*postgresstore.Store)(nil)

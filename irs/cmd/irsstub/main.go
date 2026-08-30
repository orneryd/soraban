package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"irsstub/internal/stub"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	config, err := stub.LoadConfig()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	handler, err := stub.NewServer(config, stub.Dependencies{}, logger)
	if err != nil {
		logger.Error("unable to create server", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              config.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errorsChannel := make(chan error, 1)
	go func() {
		logger.Info("IRS stub listening", "address", config.Addr)
		errorsChannel <- server.ListenAndServe()
	}()

	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errorsChannel:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	case <-shutdownContext.Done():
		context, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(context); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
	}
}

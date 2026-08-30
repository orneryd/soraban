package web

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func ListenAndServe(ctx context.Context, address string, handler http.Handler, shutdownTimeout time.Duration) error {
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errChannel := make(chan error, 1)
	go func() { errChannel <- server.ListenAndServe() }()
	select {
	case err := <-errChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

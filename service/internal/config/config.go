package config

import (
	"errors"
	"os"
	"strconv"
	"time"
)

const defaultDatabaseURL = "postgres://readiness_app:readiness-app-local-only@127.0.0.1:55432/readiness?sslmode=disable"

type Config struct {
	DatabaseURL           string
	IRSBaseURL            string
	IRSBearerToken        string
	HTTPAddr              string
	WorkerIdleDelay       time.Duration
	WorkerLeaseDuration   time.Duration
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	TotalTimeout          time.Duration
	ShutdownTimeout       time.Duration
	MaxAttempts           int
}

func Load() (Config, error) {
	config := Config{
		DatabaseURL:           env("DATABASE_URL", defaultDatabaseURL),
		IRSBaseURL:            env("IRS_BASE_URL", "http://127.0.0.1:8081"),
		IRSBearerToken:        env("IRS_BEARER_TOKEN", "local-irs-token"),
		HTTPAddr:              env("HTTP_ADDR", "127.0.0.1:8080"),
		WorkerIdleDelay:       time.Second,
		WorkerLeaseDuration:   90 * time.Second,
		ConnectTimeout:        3 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		TotalTimeout:          10 * time.Second,
		ShutdownTimeout:       15 * time.Second,
		MaxAttempts:           1000,
	}
	var err error
	for name, destination := range map[string]*time.Duration{
		"WORKER_IDLE_DELAY":            &config.WorkerIdleDelay,
		"WORKER_LEASE_DURATION":        &config.WorkerLeaseDuration,
		"HTTP_CONNECT_TIMEOUT":         &config.ConnectTimeout,
		"HTTP_RESPONSE_HEADER_TIMEOUT": &config.ResponseHeaderTimeout,
		"HTTP_TOTAL_TIMEOUT":           &config.TotalTimeout,
		"SHUTDOWN_TIMEOUT":             &config.ShutdownTimeout,
	} {
		if value, found := os.LookupEnv(name); found && value != "" {
			*destination, err = time.ParseDuration(value)
			if err != nil || *destination <= 0 {
				return Config{}, errors.New(name + " must be a positive duration")
			}
		}
	}
	if value, found := os.LookupEnv("WORKER_MAX_ATTEMPTS"); found && value != "" {
		config.MaxAttempts, err = strconv.Atoi(value)
		if err != nil || config.MaxAttempts <= 0 {
			return Config{}, errors.New("WORKER_MAX_ATTEMPTS must be a positive integer")
		}
	}
	if config.DatabaseURL == "" || config.HTTPAddr == "" {
		return Config{}, errors.New("database URL and HTTP address are required")
	}
	if config.WorkerLeaseDuration <= 60*time.Second+config.TotalTimeout+5*time.Second {
		return Config{}, errors.New("WORKER_LEASE_DURATION must exceed the 60s crash fence, HTTP timeout, and 5s persistence margin")
	}
	return config, nil
}

func env(name, fallback string) string {
	if value, found := os.LookupEnv(name); found && value != "" {
		return value
	}
	return fallback
}

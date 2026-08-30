package store

import (
	"context"

	internal "readiness.local/postgres/internal/store"
)

type Store = internal.Store

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	return internal.Open(ctx, databaseURL)
}

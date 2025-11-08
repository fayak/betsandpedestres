package logging

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

var key ctxKey

func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, key, l)
}

func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(key).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

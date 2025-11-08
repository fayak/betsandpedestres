package logging

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

type Options struct {
	Level  string // "debug"|"info"|"warn"|"error"
	Format string // "text"|"json"
}

func New(opts Options) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(opts.Level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handlerOpts := &slog.HandlerOptions{
		Level:       lvl,
		AddSource:   false,
		ReplaceAttr: replaceAttrsCompact,
	}

	var h slog.Handler
	if strings.ToLower(opts.Format) == "text" {
		h = slog.NewTextHandler(os.Stdout, handlerOpts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, handlerOpts)
	}
	return slog.New(h)
}

func replaceAttrsCompact(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		return slog.Time(slog.TimeKey, time.Now().UTC())
	}
	return a
}

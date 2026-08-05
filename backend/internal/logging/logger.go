// Package logging provides the structured slog logger used across cerbix.
package logging

import (
	"context"
	"io"
	"log/slog"

	"git.example.com/monitoring/cerbix/internal/config"
)

// LevelCritical is a custom level above Error for unrecoverable conditions.
const LevelCritical = slog.Level(12)

// New builds a slog.Logger from the log config, writing to out.
func New(cfg config.LogConfig, out io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "error":
		level = slog.LevelError
	case "critical":
		level = LevelCritical
	}
	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.LevelKey {
				if lvl, ok := attr.Value.Any().(slog.Level); ok && lvl == LevelCritical {
					attr.Value = slog.StringValue("CRITICAL")
				}
			}
			return attr
		},
	}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(out, opts))
	}
	return slog.New(slog.NewJSONHandler(out, opts))
}

// Critical logs at the custom CRITICAL level.
func Critical(logger *slog.Logger, msg string, args ...any) {
	if logger == nil {
		return
	}
	logger.Log(context.Background(), LevelCritical, msg, args...)
}

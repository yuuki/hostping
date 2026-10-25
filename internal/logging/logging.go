// Package logging configures the zerolog logger shared by both binaries.
package logging

import (
	"log/slog"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Setup installs timestamped console output on stderr, as in rpingmesh.
func Setup() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, NoColor: true}).
		With().Timestamp().Logger().Level(zerolog.InfoLevel)
}

// Infof adapts the existing injectable log callbacks to zerolog's info level.
// zerolog's Printf uses debug level, which would hide operator messages.
func Infof(format string, args ...any) {
	log.Info().Msgf(format, args...)
}

// Slog adapts dependencies that require slog to the same zerolog output.
func Slog() *slog.Logger {
	return slog.New(zerolog.NewSlogHandler(log.Logger))
}

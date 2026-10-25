package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestInfofAtInfoLevel(t *testing.T) {
	var out bytes.Buffer
	before := log.Logger
	t.Cleanup(func() { log.Logger = before })
	log.Logger = zerolog.New(&out).Level(zerolog.InfoLevel)
	Infof("hostping: %s paths=%d", "ready", 3)
	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["level"] != "info" || entry["message"] != "hostping: ready paths=3" {
		t.Fatalf("operator log lost its level or message: %v", entry)
	}
}

func TestSlogUsesZerolog(t *testing.T) {
	var out bytes.Buffer
	before := log.Logger
	t.Cleanup(func() { log.Logger = before })
	log.Logger = zerolog.New(&out).With().Timestamp().Logger()
	Slog().With("component", "metrics").Warn("TLS disabled", "address", ":9109")
	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["level"] != "warn" || entry["message"] != "TLS disabled" ||
		entry["component"] != "metrics" || entry["address"] != ":9109" || entry["time"] == nil {
		t.Fatalf("dependency log lost its fields: %v", entry)
	}
	out.Reset()
	slog.NewLogLogger(Slog().Handler(), slog.LevelError).Print("HTTP error")
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["level"] != "error" || entry["message"] != "HTTP error" {
		t.Fatalf("HTTP log lost its level or message: %v", entry)
	}
}

func TestSetupWritesConsoleToStderr(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	before, stderr := log.Logger, os.Stderr
	t.Cleanup(func() { log.Logger, os.Stderr = before, stderr })
	os.Stderr = f
	Setup()
	Infof("hostping ready")
	log.Debug().Msg("hidden debug message")
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	line := string(data)
	if !strings.Contains(line, "INF hostping ready") || strings.Contains(line, "\x1b") || strings.Count(line, "\n") != 1 {
		t.Fatalf("unexpected console output: %q", line)
	}
	if _, err := time.Parse(time.Kitchen, strings.Fields(line)[0]); err != nil {
		t.Fatalf("missing timestamp: %q", line)
	}
}

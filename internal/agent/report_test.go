package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/buildinfo"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
)

var testReportTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testReporter(t *testing.T, format config.ReportFormat, file string, stdout io.Writer) *reporter {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &reporter{
		format: format,
		file:   file,
		stdout: stdout,
		build:  buildinfo.Info{Version: "1.2.3", Commit: "abc1234"},
		now:    func() time.Time { return testReportTime },
	}
}

func testReport() model.Report {
	return model.Report{
		Trigger: model.TriggerIdle,
		Mode:    model.ModeIdle,
		Host:    "h",
		Started: testReportTime.Add(-time.Minute),
		Paths:   make([]model.PathMeasurement, 2),
		Causes: []model.RootCause{{
			Code: "rnic_pcie_failure", Confidence: "high",
			Summary: "multi\nline summary", Evidence: "evidence",
		}},
	}
}

// A log pipeline reads one entry per line: each report must be exactly one
// line of valid JSON, carrying the existing fields under their names plus the
// time and build it was written by.
func TestCompactReportIsOneJSONLine(t *testing.T) {
	var out bytes.Buffer
	rp := testReporter(t, config.ReportFormatJSON, "", &out)
	for range 2 {
		if err := rp.emit(testReport()); err != nil {
			t.Fatal(err)
		}
	}
	got := out.String()
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("output not newline-terminated: %q", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines for 2 reports:\n%s", len(lines), got)
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line is not JSON: %v: %s", err, l)
		}
		for _, k := range []string{"trigger", "mode", "host", "started", "elapsed_ns", "paths", "links", "causes", "host_idle"} {
			if _, ok := m[k]; !ok {
				t.Errorf("field %q missing from %s", k, l)
			}
		}
		if m["reported_at"] != "2026-01-02T03:04:05Z" || m["version"] != "1.2.3" || m["commit"] != "abc1234" {
			t.Errorf("reported_at/version/commit = %v/%v/%v", m["reported_at"], m["version"], m["commit"])
		}
	}
}

// pretty keeps the indented layout an operator reads in a terminal.
func TestPrettyReportIsIndentedJSON(t *testing.T) {
	var out bytes.Buffer
	rp := testReporter(t, config.ReportFormatPretty, "", &out)
	rep := testReport()
	if err := rp.emit(rep); err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(reportRecord{Report: rep, ReportedAt: testReportTime, Version: "1.2.3", Commit: "abc1234"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != string(want)+"\n" {
		t.Fatalf("pretty output:\n%s\nwant:\n%s", got, want)
	}
}

// The report file gets one compact line per report, appended to what is
// already there, and none keeps stdout empty while the file is still written.
func TestReportFileAppendsLinesAndNoneSilencesStdout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.jsonl")
	if err := os.WriteFile(path, []byte("{\"earlier\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rp := testReporter(t, config.ReportFormatNone, path, &out)
	for range 2 {
		if err := rp.emit(testReport()); err != nil {
			t.Fatal(err)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("--report-format=none wrote to stdout: %q", out.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "{\"earlier\":true}" {
		t.Fatalf("file has %d lines, want the earlier one plus 2:\n%s", len(lines), data)
	}
	for _, l := range lines[1:] {
		if !json.Valid([]byte(l)) {
			t.Fatalf("file line is not JSON: %s", l)
		}
	}
}

// The file is reopened per report, so a rotated-away file is recreated.
func TestReportFileIsRecreatedAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.jsonl")
	rp := testReporter(t, config.ReportFormatNone, path, io.Discard)
	if err := rp.emit(testReport()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := rp.emit(testReport()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("new file has %d lines, want 1", n)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&^reportFileMode != 0 {
		t.Fatalf("report file mode %v is wider than %v", perm, os.FileMode(reportFileMode))
	}
}

// A report file that cannot be opened fails the start, not the first report.
func TestNewReporterRejectsUnopenableFile(t *testing.T) {
	cfg := config.Defaults()
	cfg.ReportFile = filepath.Join(t.TempDir(), "missing-dir", "reports.jsonl")
	if _, err := newReporter(cfg, io.Discard, buildinfo.Info{}); err == nil || !strings.Contains(err.Error(), "--report-file") {
		t.Fatalf("err = %v, want a --report-file error", err)
	}
}

// End to end: a one-shot sim run with --report-format=none and --report-file
// writes nothing to stdout and one line to the file.
func TestRunOnceWritesReportFile(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.Once = true
	cfg.ReportFormat = config.ReportFormatNone
	cfg.ReportFile = filepath.Join(t.TempDir(), "reports.jsonl")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	log.SetOutput(io.Discard)
	runErr := Run(cfg)
	os.Stdout = stdout
	log.SetOutput(os.Stderr)
	_ = w.Close()
	printed, _ := io.ReadAll(r)
	_ = r.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(printed) != 0 {
		t.Fatalf("--report-format=none printed %d bytes to stdout", len(printed))
	}
	data, err := os.ReadFile(cfg.ReportFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 || !json.Valid(bytes.TrimSpace(data)) {
		t.Fatalf("report file is not one JSON line:\n%s", data)
	}
}

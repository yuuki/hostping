package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/buildinfo"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
)

// reportFileMode is the permission a new --report-file is created with: the
// report names the host's devices and faults, which is not for every user.
const reportFileMode = 0o640

// reportRecord is what a report is written as: the report's own fields,
// unchanged, plus when it was written and by which build, so a line read back
// from a log pipeline or the report file stands on its own. "started" is when
// the run began; "reported_at" can be a poll later for a held idle-mode run.
type reportRecord struct {
	model.Report
	ReportedAt time.Time `json:"reported_at"`
	Version    string    `json:"version"`
	Commit     string    `json:"commit"`
}

// reporter writes each confirmed report to stdout in the configured format,
// appends it to the report file when one is set, and logs its baselines and
// diagnoses. A report that is never handed to it (a discarded idle-mode run)
// reaches neither output.
type reporter struct {
	format config.ReportFormat
	file   string
	stdout io.Writer
	build  buildinfo.Info
	now    func() time.Time
	// logf receives the baselines, diagnoses and write failures.
	logf func(format string, args ...any)
}

// newReporter checks that the report file can be opened for appending, so a
// wrong path fails at start-up rather than at the first report.
func newReporter(cfg config.Config, stdout io.Writer, build buildinfo.Info) (*reporter, error) {
	r := &reporter{
		format: cfg.EffectiveReportFormat(),
		file:   cfg.ReportFile,
		stdout: stdout,
		build:  build,
		now:    time.Now,
		logf:   log.Printf,
	}
	if r.file != "" {
		f, err := openReportFile(r.file)
		if err != nil {
			return nil, fmt.Errorf("--report-file: %w", err)
		}
		_ = f.Close()
	}
	return r, nil
}

func openReportFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, reportFileMode)
}

// emit writes rep to every output. A failed write is logged and returned; the
// remaining outputs are still attempted.
func (r *reporter) emit(rep model.Report) error {
	if rep.Causes == nil {
		rep.Causes = []model.RootCause{}
	}
	rec := reportRecord{
		Report:     rep,
		ReportedAt: r.now().UTC(),
		Version:    r.build.Version,
		Commit:     r.build.Commit,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		r.logf("hostping: report json: %v", err)
		return err
	}
	line = append(line, '\n')

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	switch r.format {
	case config.ReportFormatJSON:
		if _, err := r.stdout.Write(line); err != nil {
			r.logf("hostping: report stdout: %v", err)
			keep(err)
		}
	case config.ReportFormatPretty:
		var buf bytes.Buffer
		if err := json.Indent(&buf, line, "", "  "); err != nil {
			keep(err)
		} else if _, err := r.stdout.Write(buf.Bytes()); err != nil {
			r.logf("hostping: report stdout: %v", err)
			keep(err)
		}
	}
	if r.file != "" {
		if err := appendLine(r.file, line); err != nil {
			r.logf("hostping: report file: %v", err)
			keep(err)
		}
	}

	for _, t := range rep.Baselines {
		r.logf("hostping baseline %s", baseline.FormatTable(t))
	}
	if len(rep.Causes) == 0 {
		r.logf("hostping: no intra-host bottleneck (trigger=%s mode=%s paths=%d%s)", rep.Trigger, rep.Mode, len(rep.Paths), skippedSuffix(rep))
		return firstErr
	}
	for _, c := range rep.Causes {
		r.logf("hostping diagnosis [%s] %s (%s): %s", c.Confidence, c.Code, c.Summary, c.Evidence)
	}
	return firstErr
}

// appendLine opens the file for every report rather than holding it open, so
// a rotated file is replaced on the next report without a signal. The line is
// one write on an O_APPEND descriptor: concurrent appenders do not interleave
// within it.
func appendLine(path string, line []byte) error {
	f, err := openReportFile(path)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

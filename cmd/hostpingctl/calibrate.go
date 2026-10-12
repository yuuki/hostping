package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yuuki/hostping/internal/agent"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/calibrate"
	"github.com/yuuki/hostping/internal/config"
)

// runCalibrate is hostpingctl calibrate [merge]. It writes the baseline file
// to --out (or stdout) and the summary table to stderr.
func runCalibrate(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "merge" {
		return runCalibrateMerge(args[1:], stdout, stderr)
	}
	cfg := config.Defaults()
	fs := flag.NewFlagSet("hostpingctl calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg.RegisterFlags(fs)
	opt := agent.CalibrateOptions{Runs: 5, Interval: 30 * time.Second, MaxDiscards: 3}
	fs.IntVar(&opt.Runs, "runs", opt.Runs, "idle-mode runs to calibrate from")
	fs.DurationVar(&opt.Interval, "interval", opt.Interval, "pause between runs")
	fs.IntVar(&opt.MaxDiscards, "max-discards", opt.MaxDiscards, "runs that may be discarded because the host became busy before calibration fails")
	out := fs.String("out", "", "write the baseline file here instead of stdout")
	lat := fs.Bool("latency", false, "also set a latency baseline per distance (strict: a set latency has no 500 ns floor; see README)")
	if err := cfg.Parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q (did you mean: hostpingctl calibrate merge FILE...?)", fs.Args())
	}
	cr, err := agent.Calibrate(cfg, opt)
	if err != nil {
		return err
	}
	res := calibrate.Summarize(cr.Graph, cr.Reports, calibrate.Options{Latency: *lat, AbnormalTh: cfg.AbnormalTh})
	res.Discarded = cr.Discarded
	data, err := calibrate.MarshalFile(res.Baselines())
	if err != nil {
		return err
	}
	if err := writeOut(*out, data, stdout); err != nil {
		return err
	}
	res.WriteTable(stderr)
	return nil
}

func runCalibrateMerge(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hostpingctl calibrate merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write the merged baseline file here instead of stdout")
	th := fs.Float64("abnormal-th", config.Defaults().AbnormalTh, "warn about inputs this far below the merged bandwidth")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("calibrate merge needs at least one baseline file (hostpingctl calibrate merge [--out FILE] FILE ...)")
	}
	in := make([]calibrate.MergeInput, 0, fs.NArg())
	for _, path := range fs.Args() {
		b, err := baseline.LoadFile(path)
		if err != nil {
			return err
		}
		in = append(in, calibrate.MergeInput{Name: path, Bases: b})
	}
	res, err := calibrate.Merge(in, *th)
	if err != nil {
		return err
	}
	data, err := calibrate.MarshalFile(res.Baselines())
	if err != nil {
		return err
	}
	if err := writeOut(*out, data, stdout); err != nil {
		return err
	}
	res.WriteTable(stderr)
	return nil
}

func writeOut(path string, data []byte, stdout io.Writer) error {
	if path == "" {
		_, err := stdout.Write(data)
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

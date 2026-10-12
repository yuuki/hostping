package config

import (
	"flag"
	"io"
	"testing"
)

func TestParseCapacity(t *testing.T) {
	for in, want := range map[string]uint64{
		"400Gbps":      400e9,
		"400gbps":      400e9,
		"400Gb/s":      400e9,
		"1_000Mbps":    1e9,
		"50GB/s":       400e9,
		"50GBps":       400e9,
		"1GiB/s":       8 << 30,
		"1.5TB/s":      12e12,
		"300000000bps": 3e8,
		"2kB/s":        16e3,
	} {
		got, err := ParseCapacity(in)
		if err != nil || got != want {
			t.Errorf("ParseCapacity(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "400", "50GB", "400G", "0Gbps", "-1GB/s", "xGbps", "1e30TB/s"} {
		if got, err := ParseCapacity(in); err == nil {
			t.Errorf("ParseCapacity(%q) = %d, want an error", in, got)
		}
	}
}

func TestCapacityFlags(t *testing.T) {
	cfg := Defaults()
	if cfg.MemCapacity.Set() || cfg.UPICapacity.Set() || cfg.UncoreUtil {
		t.Fatal("capacities and the uncore reader must be off by default")
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{
		"--mem-channel-capacity", "200GB/s",
		"--mem-channel-capacity", "mem.1=100GB/s",
		"--upi-capacity", "upi.0-1=384Gbps",
		"--uncore-util",
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.MemCapacity.All != 1600e9 || cfg.MemCapacity.ByLink["mem.1"] != 800e9 {
		t.Fatalf("mem %+v", cfg.MemCapacity)
	}
	if cfg.UPICapacity.All != 0 || cfg.UPICapacity.ByLink["upi.0-1"] != 384e9 {
		t.Fatalf("upi %+v", cfg.UPICapacity)
	}
	if !cfg.UncoreUtil {
		t.Fatal("--uncore-util not set")
	}
}

func TestCapacityFlagsRejectBadValues(t *testing.T) {
	for _, args := range [][]string{
		{"--mem-channel-capacity", "200GB"},
		{"--mem-channel-capacity", "upi.0-1=200GB/s"},
		{"--mem-channel-capacity", "mem.=200GB/s"},
		{"--upi-capacity", "mem.0=200GB/s"},
	} {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		cfg.RegisterFlags(fs)
		if err := fs.Parse(args); err == nil {
			t.Errorf("%v must fail", args)
		}
	}
}

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/rs/zerolog/log"

	"github.com/yuuki/hostping/internal/agent"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/logging"
	"github.com/yuuki/hostping/internal/zigprobe"
)

func main() {
	logging.Setup()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	if cmd == "calibrate" {
		if err := runCalibrate(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				os.Exit(0)
			}
			log.Fatal().Err(err).Msg("hostpingctl failed")
		}
		return
	}
	cfg := config.Defaults()
	fs := flag.NewFlagSet("hostpingctl "+cmd, flag.ExitOnError)
	cfg.RegisterFlags(fs)
	_ = cfg.Parse(fs, os.Args[2:])
	cfg.Once = true
	switch cmd {
	case "diagnose", "once":
		if err := agent.Run(cfg); err != nil {
			log.Fatal().Err(err).Msg("hostpingctl failed")
		}
	case "topology":
		if err := agent.PrintTopology(cfg); err != nil {
			log.Fatal().Err(err).Msg("hostpingctl failed")
		}
	case "version":
		fmt.Println(agent.BuildInfo().Line("hostpingctl"))
	case "eq":
		lat := zigprobe.SimLatency(400, 600, 128*1024, 200_000_000_000)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"large_msg_latency_ns": lat, "via": "zig C ABI"})
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `hostpingctl — Hostping (NSDI'23 / ToN'24) operator CLI

Usage:
  hostpingctl diagnose --sim [--fault rnic_pcie:rnic0]
  hostpingctl diagnose --baseline local_mem=270Gbps,1400ns
  hostpingctl diagnose --baseline-file baselines.json
  hostpingctl diagnose              # live sysfs + ibverbs; fails closed without RNICs
  hostpingctl calibrate [--runs 5] [--interval 30s] [--latency] [--out FILE]
                                    # idle runs on a healthy host -> --baseline-file JSON
  hostpingctl calibrate merge [--out FILE] FILE...
                                    # one file per hardware class (lower quartile)
  hostpingctl topology --sim
  hostpingctl topology              # discovered PCI graph
  hostpingctl version               # version, commit, zig probe and Go versions
  hostpingctl eq
`)
}

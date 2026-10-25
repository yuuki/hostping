package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rs/zerolog/log"

	"github.com/yuuki/hostping/internal/agent"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/logging"
)

func main() {
	logging.Setup()
	cfg := config.Defaults()
	fs := flag.NewFlagSet("hostping", flag.ExitOnError)
	cfg.RegisterFlags(fs)
	showVersion := fs.Bool("version", false, "print the version and exit")
	_ = cfg.Parse(fs, os.Args[1:])
	// Before agent.Run: --version must not discover hardware or bind ports.
	if *showVersion {
		fmt.Println(agent.BuildInfo().Line("hostping"))
		return
	}
	if err := agent.Run(cfg); err != nil {
		log.Fatal().Err(err).Msg("hostping failed")
	}
}

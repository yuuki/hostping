package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/yuuki/hostping/internal/agent"
	"github.com/yuuki/hostping/internal/config"
)

func main() {
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
		log.Fatal(err)
	}
}

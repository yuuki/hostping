package config

import (
	"flag"
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func TestDropFlags(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{"--drop-ignore-rnic", "mlx5_3,rnic9", "--lossless-priorities", "4, 3", "--lossless-priorities", "3"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"mlx5_3", "rnic9"}; !reflect.DeepEqual(cfg.DropIgnoreRNICs, want) {
		t.Errorf("DropIgnoreRNICs = %q, want %q", cfg.DropIgnoreRNICs, want)
	}
	if want := []int{3, 4}; !reflect.DeepEqual(cfg.LosslessPriorities, want) {
		t.Errorf("LosslessPriorities = %v, want %v (sorted, unique)", cfg.LosslessPriorities, want)
	}
	if !cfg.Lossless(3) || cfg.Lossless(0) {
		t.Error("only the listed priorities are lossless")
	}
	if !Defaults().Lossless(0) {
		t.Error("with no --lossless-priorities every priority counts as lossless")
	}
}

func TestDropIgnoredMatchesIDDeviceOrPCIAddress(t *testing.T) {
	rnics := []model.RNIC{
		{ID: "rnic0", Device: "mlx5_0", PCIeBDF: "0000:1a:00.0"},
		{ID: "rnic1", Device: "mlx5_1", PCIeBDF: "0000:3c:00.0"},
		{ID: "rnic2", Device: "mlx5_2"},
	}
	cfg := Defaults()
	cfg.DropIgnoreRNICs = []string{"rnic0", "mlx5_2", "0000:3C:00.0"}
	for _, n := range rnics {
		if !cfg.DropIgnored(n) {
			t.Errorf("%s not ignored", n.ID)
		}
	}
	if err := cfg.ValidateDropIgnore(rnics); err != nil {
		t.Fatal(err)
	}
	cfg.DropIgnoreRNICs = []string{"rnic1", "mlx5_9"}
	if cfg.DropIgnored(rnics[0]) {
		t.Error("rnic0 is not named")
	}
	err := cfg.ValidateDropIgnore(rnics)
	if err == nil || !strings.Contains(err.Error(), `"mlx5_9"`) {
		t.Fatalf("ValidateDropIgnore = %v, want an error naming mlx5_9", err)
	}
}

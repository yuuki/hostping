package model

import "testing"

func TestParseDistance(t *testing.T) {
	for _, d := range Distances() {
		got, err := ParseDistance(d.Key())
		if err != nil || got != d {
			t.Fatalf("%s → %d (%v)", d.Key(), got, err)
		}
	}
	if _, err := ParseDistance("nope"); err == nil {
		t.Fatal("unknown key")
	}
}

package monitor

import "testing"

func TestNormalizeBDFDomainWidth(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0000:18:00.0", "18:00.0"},
		{"00000000:18:00.0", "18:00.0"},
		{"18:00.0", "18:00.0"},
		{"0000:3C:00.1", "3c:00.1"},
	}
	for _, tc := range cases {
		if got := normalizeBDF(tc.in); got != tc.want {
			t.Fatalf("normalizeBDF(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

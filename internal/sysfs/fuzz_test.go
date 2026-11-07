package sysfs

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// FuzzParseCPUList: a cpulist either fails or yields CPU IDs in
// [0, maxCPUListLen), at most maxCPUListLen of them, and the list it yields
// reads back the same from its plain "a,b,c" spelling. The parse is bounded
// whatever the input: a range is materialized CPU by CPU.
func FuzzParseCPUList(f *testing.F) {
	for _, s := range []string{
		"0-2,8,10-11\n", "0,56", "0-127", "", " \n", "3-1", "1-", "-1", "0,",
		"+1-+3", "0-1048575", "0-1048576", "0-9223372036854775807",
		"0-524287,0-524287,0", "9223372036854775807",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := ParseCPUList(s)
		if err != nil {
			if out != nil {
				t.Fatalf("ParseCPUList(%q) = %v with error %v", s, out, err)
			}
			return
		}
		if len(out) > maxCPUListLen {
			t.Fatalf("ParseCPUList(%q) returned %d CPUs, more than %d", s, len(out), maxCPUListLen)
		}
		parts := make([]string, len(out))
		for i, c := range out {
			if c < 0 || c >= maxCPUListLen {
				t.Fatalf("ParseCPUList(%q) returned CPU %d", s, c)
			}
			parts[i] = strconv.Itoa(c)
		}
		again, err := ParseCPUList(strings.Join(parts, ","))
		if err != nil || !slices.Equal(again, out) {
			t.Fatalf("ParseCPUList(%q) = %v, which reads back as %v, %v", s, out, again, err)
		}
	})
}

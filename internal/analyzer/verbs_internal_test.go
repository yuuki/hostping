package analyzer

import "testing"

func TestLowerMedianU64(t *testing.T) {
	for _, tc := range []struct {
		in   []uint64
		want uint64
	}{
		{[]uint64{7}, 7},
		{[]uint64{9, 3}, 3},
		{[]uint64{5, 1, 9}, 5},
		{[]uint64{40, 1, 2, 30}, 2},
		{[]uint64{1, 50, 2, 40, 30}, 30},
	} {
		in := append([]uint64(nil), tc.in...)
		if got := lowerMedianU64(tc.in); got != tc.want {
			t.Errorf("lowerMedianU64(%v) = %d, want %d", tc.in, got, tc.want)
		}
		for i := range in {
			if in[i] != tc.in[i] {
				t.Fatalf("lowerMedianU64 reordered its input: %v", tc.in)
			}
		}
	}
}

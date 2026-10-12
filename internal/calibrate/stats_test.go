package calibrate

import "testing"

func TestQuantileSmallN(t *testing.T) {
	cases := []struct {
		v    []uint64
		q    float64
		want uint64
	}{
		{nil, 0.5, 0},
		{[]uint64{7}, 0.25, 7},
		{[]uint64{7}, 0.5, 7},
		{[]uint64{7}, 0.75, 7},
		{[]uint64{10, 20}, 0.5, 15},
		{[]uint64{20, 10}, 0.25, 13}, // 10 + 0.25*10 = 12.5, rounded half away from zero
		{[]uint64{10, 20}, 0.75, 18}, // 17.5
		{[]uint64{30, 10, 20}, 0.5, 20},
		{[]uint64{30, 10, 20}, 0.25, 15},
		{[]uint64{40, 10, 30, 20}, 0.5, 25},
		{[]uint64{40, 10, 30, 20}, 0.25, 18}, // 17.5
		{[]uint64{40, 10, 30, 20}, 0.75, 33}, // 32.5
		{[]uint64{50, 10, 40, 20, 30}, 0.25, 20},
		{[]uint64{50, 10, 40, 20, 30}, 0, 10},
		{[]uint64{50, 10, 40, 20, 30}, 1, 50},
	}
	for _, c := range cases {
		if got := Quantile(c.v, c.q); got != c.want {
			t.Errorf("Quantile(%v, %v) = %d, want %d", c.v, c.q, got, c.want)
		}
	}
}

func TestQuantileDoesNotReorderInput(t *testing.T) {
	v := []uint64{3, 1, 2}
	_ = Median(v)
	if v[0] != 3 || v[1] != 1 || v[2] != 2 {
		t.Fatalf("input reordered: %v", v)
	}
}

// The median of real readings is robust: one wild value, high or low, does
// not move it.
func TestMedianRobustToOneOutlier(t *testing.T) {
	healthy := []uint64{196, 197, 198, 197, 196}
	if got := Median(healthy); got != 197 {
		t.Fatalf("median = %d", got)
	}
	if got := Median(append(healthy[:4:4], 3)); got != 197 {
		t.Fatalf("median with a low outlier = %d", got)
	}
	if got := Median(append(healthy[:4:4], 1_000_000)); got != 197 {
		t.Fatalf("median with a high outlier = %d", got)
	}
}

func TestSpreadOf(t *testing.T) {
	if s := SpreadOf(nil); s != (Spread{}) {
		t.Fatalf("empty spread = %+v", s)
	}
	s := SpreadOf([]uint64{50, 10, 40, 20, 30})
	want := Spread{N: 5, Min: 10, P25: 20, Median: 30, P75: 40, Max: 50}
	if s != want {
		t.Fatalf("spread = %+v, want %+v", s, want)
	}
}

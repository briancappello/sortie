package huly

import "testing"

// The cases below are @hcengineering/rank's own makeRank tests.

func mustRank(t *testing.T, prev, next string) string {
	t.Helper()
	r, err := makeRank(prev, next)
	if err != nil {
		t.Fatalf("makeRank(%q, %q) error = %v", prev, next, err)
	}
	return r
}

func TestMakeRankNoNeighbours(t *testing.T) {
	t.Parallel()
	for _, next := range []string{"", " "} {
		if got := mustRank(t, "", next); got != "0|hzzzzz:" {
			t.Errorf("makeRank(\"\", %q) = %q, want 0|hzzzzz:", next, got)
		}
	}
}

func TestMakeRankTable(t *testing.T) {
	t.Parallel()

	tests := []struct{ prev, next, want string }{
		{"0|hzzzzz:", "", "0|i00007:"},
		{"0|i00007:", "", "0|i0000f:"},
		{"0|i0000f:", "", "0|i0000n:"},
		{"0|zzzzzz:", "", "0|zzzzzz:"},
		{"", "0|hzzzzz:", "0|hzzzzr:"},
		{"", "0|hzzzzr:", "0|hzzzzj:"},
		{"", "0|hzzzzj:", "0|hzzzzb:"},
		{"", "0|000000:", "0|000000:"},
		{"0|000000:", "0|000001:", "0|000000:i"},
		{"0|hzzzzz:", "0|i0000f:", "0|i00007:"},
		{"0|hzzzzz:", "0|hzzzzz:", "0|i00007:"},
		{"0|i00007:", "0|i00007:", "0|i0000f:"},
		{"0|i00007:", "0|i00008:", "0|i00007:i"},
	}
	for _, tt := range tests {
		if got := mustRank(t, tt.prev, tt.next); got != tt.want {
			t.Errorf("makeRank(%q, %q) = %q, want %q", tt.prev, tt.next, got, tt.want)
		}
	}
}

func TestMakeRankGenerations(t *testing.T) {
	t.Parallel()

	repeat := func(n int, step func(string) string) string {
		r := "0|hzzzzz:"
		for range n {
			r = step(r)
		}
		return r
	}
	prev := func(r string) string { return mustRank(t, "", r) }
	middle := func(r string) string { return mustRank(t, r, "0|zzzzzz") }
	next := func(r string) string { return mustRank(t, r, "") }

	tests := []struct {
		name string
		n    int
		step func(string) string
		want string
	}{
		{"prev", 10, prev, "0|hzzzxr:"},
		{"prev", 100, prev, "0|hzzzdr:"},
		{"prev", 1000, prev, "0|hzzttr:"},
		{"prev", 10000, prev, "0|hzya9r:"},
		{"middle", 5, middle, "0|zfqzzz:"},
		{"middle", 10, middle, "0|zzd7vh:"},
		{"middle", 50, middle, "0|zzzzzy:zzzi"},
		{"middle", 100, middle, "0|zzzzzy:zzzzzzzzzzzv"},
		{"next", 10, next, "0|i00027:"},
		{"next", 100, next, "0|i000m7:"},
		{"next", 1000, next, "0|i00667:"},
		{"next", 10000, next, "0|i01pq7:"},
		{"next", 100000, next, "0|i0h5a7:"},
	}
	for _, tt := range tests {
		if got := repeat(tt.n, tt.step); got != tt.want {
			t.Errorf("%d %s generations = %q, want %q", tt.n, tt.name, got, tt.want)
		}
	}
}

func TestMakeRankRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"hzzzzz", "0|hz!zz:", "7|hzzzzz:", "0|a:b:c"} {
		if _, err := makeRank(bad, ""); err == nil {
			t.Errorf("makeRank(%q) error = nil, want an error", bad)
		}
	}
}

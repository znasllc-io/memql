package actionsearch

import (
	"encoding/json"
	"testing"
)

func TestVectorLiteral(t *testing.T) {
	if got := vectorLiteral(nil); got != "[]" {
		t.Fatalf("empty: want [], got %s", got)
	}
	if got := vectorLiteral([]float32{0.5, -1, 2}); got != "[0.5,-1,2]" {
		t.Fatalf("want [0.5,-1,2], got %s", got)
	}
}

func TestToInt(t *testing.T) {
	cases := []struct {
		in   any
		want int
		ok   bool
	}{
		{float64(5), 5, true},
		{int(3), 3, true},
		{int64(7), 7, true},
		{json.Number("9"), 9, true},
		{"x", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := toInt(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("toInt(%v): want (%d,%v), got (%d,%v)", c.in, c.want, c.ok, got, ok)
		}
	}
}

package decisions

import "testing"

func TestRingNewestFirstAndBounded(t *testing.T) {
	r := NewRing(3)
	var seen int
	r.OnAdd(func(Decision) { seen++ })
	if len(r.List()) != 0 {
		t.Fatal("not empty")
	}
	for _, a := range []string{"1", "2", "3", "4"} {
		r.Add(Decision{Action: a})
	}
	l := r.List()
	if len(l) != 3 || l[0].Action != "4" || l[2].Action != "2" {
		t.Fatalf("got %+v", l)
	}
	if seen != 4 {
		t.Fatal("callback not run")
	}
}

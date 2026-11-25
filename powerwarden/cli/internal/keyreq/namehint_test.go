package keyreq

import (
	"os"
	"strings"
	"testing"
)

// With no $USER the owner guess is "@<host>": the hint is the short host
// alone, never "@host" and never empty.
func TestNameHintWithoutUser(t *testing.T) {
	h, _ := os.Hostname()
	short, _, _ := strings.Cut(h, ".")
	for _, guess := range []string{"@" + short, "@box", ""} {
		got := Options(Params{NameHint: guess}).NameHint
		want := strings.TrimPrefix(guess, "@")
		if want == "" {
			want = short
		}
		if got != want || strings.HasPrefix(got, "@") || got == "" {
			t.Errorf("guess %q: hint %q, want %q", guess, got, want)
		}
	}
	if got := Options(Params{NameHint: "tester@box"}).NameHint; got != "tester@box" {
		t.Fatalf("%q", got)
	}
}

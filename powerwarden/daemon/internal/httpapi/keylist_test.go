package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/keyclient/keylist"
	api "github.com/resurgentech/powerwarden/powerwarden/api"
)

func hexOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestSR4TakenLocally(t *testing.T) {
	toks, err := NewTokenEntries(map[string]TokenEntry{
		"hourchd":    {Token: "hourchd-token-0123456789"},
		"operator@desk": {Token: "oper-agent-token-0123456", Role: RoleAgent},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		e    keylist.Entry
		want bool
	}{
		{"name taken", keylist.Entry{Name: "hourchd", Role: "full", SHA256: hexOf("other-key-0123456789")}, true},
		{"agent name taken", keylist.Entry{Name: "operator@desk", Role: "full", SHA256: hexOf("other-key-0123456789")}, true},
		{"hash taken", keylist.Entry{Name: "pulled", Role: "full", SHA256: hexOf("oper-agent-token-0123456")}, true},
		{"free", keylist.Entry{Name: "pulled", Role: "full", SHA256: hexOf("other-key-0123456789")}, false},
		{"undecodable hash is not taken", keylist.Entry{Name: "pulled", SHA256: "zz"}, false},
		{"short hash is not taken", keylist.Entry{Name: "pulled", SHA256: "abcd"}, false},
		{"upper hex decodes", keylist.Entry{Name: "pulled", SHA256: strings.ToUpper(hexOf("hourchd-token-0123456789"))}, true},
	}
	for _, c := range cases {
		if got := toks.TakenLocally(c.e); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSR7PersonalKeysStatusMapping(t *testing.T) {
	lt := time.Date(2026, 11, 19, 12, 0, 0, 123, time.FixedZone("x", 3600))
	ls := lt.Add(time.Minute)

	none := personalKeysStatus(keylist.Status{Origin: keylist.OriginNone})
	if none.Origin != api.None || none.Entries != 0 || none.AgeSeconds != nil || none.ListTime != nil ||
		none.LastSuccess != nil || none.LastError != nil || none.CacheFailing {
		t.Fatalf("origin none: %+v", none)
	}

	got := personalKeysStatus(keylist.Status{
		Origin: keylist.OriginProvider, Entries: 3, Age: 90*time.Second + 999*time.Millisecond,
		ListTime: lt, LastSuccess: ls, CacheFailing: true,
	})
	if got.Origin != api.Provider || got.Entries != 3 || !got.CacheFailing {
		t.Fatalf("%+v", got)
	}
	if got.AgeSeconds == nil || *got.AgeSeconds != 90 {
		t.Fatalf("age truncates: %v", got.AgeSeconds)
	}
	if got.ListTime == nil || !got.ListTime.Equal(lt) || got.ListTime.Location() != lt.Location() {
		t.Fatalf("list_time: %v", got.ListTime)
	}
	if got.LastSuccess == nil || !got.LastSuccess.Equal(ls) {
		t.Fatalf("last_success: %v", got.LastSuccess)
	}
	if got.LastError != nil {
		t.Fatalf("last_error: %v", *got.LastError)
	}

	// Cache origin: last_success zero is absent, list_time and age present.
	c := personalKeysStatus(keylist.Status{Origin: keylist.OriginCache, Entries: 1, Age: time.Hour, ListTime: lt})
	if c.Origin != api.Cache || c.LastSuccess != nil || c.ListTime == nil || c.AgeSeconds == nil || *c.AgeSeconds != 3600 {
		t.Fatalf("cache: %+v", c)
	}

	// last_error: non-printable replaced by ?, cut to 200 runes, no ellipsis.
	e := personalKeysStatus(keylist.Status{Origin: keylist.OriginNone, LastError: errors.New("bad\nbody\x00\x1b[31m​ ok é")})
	if e.LastError == nil || *e.LastError != "bad?body??[31m? ok é" {
		t.Fatalf("sanitised: %q", deref(e.LastError))
	}
	long := strings.Repeat("é", 300)
	e = personalKeysStatus(keylist.Status{Origin: keylist.OriginNone, LastError: errors.New(long)})
	if e.LastError == nil || len([]rune(*e.LastError)) != 200 || *e.LastError != strings.Repeat("é", 200) {
		t.Fatalf("cut: %d runes", len([]rune(deref(e.LastError))))
	}
	short := strings.Repeat("a", 200)
	e = personalKeysStatus(keylist.Status{Origin: keylist.OriginNone, LastError: errors.New(short)})
	if *e.LastError != short {
		t.Fatal("200 runes must stay whole")
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

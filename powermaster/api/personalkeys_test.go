package powermasterapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"
)

// TestPersonalKeysStatusSchema checks the key-list health object of GET
// /v1/stats: the PersonalKeysStatus schema is the shared one (the same in every
// daemon spec), Stats carries it as an optional personal_keys, and the
// generated types round-trip it.
func TestPersonalKeysStatusSchema(t *testing.T) {
	file, embedded := loadSpecs(t)
	if embedded.Components.Schemas["PersonalKeysStatus"] == nil {
		t.Fatal("PersonalKeysStatus missing from the embedded spec; run `just generate`")
	}
	ref := file.Components.Schemas["PersonalKeysStatus"]
	if ref == nil || ref.Value == nil {
		t.Fatal("schema PersonalKeysStatus missing")
	}
	s := ref.Value
	if !s.Type.Is("object") {
		t.Errorf("type %v, want object", s.Type)
	}
	req := append([]string(nil), s.Required...)
	sort.Strings(req)
	if !reflect.DeepEqual(req, []string{"cache_failing", "entries", "origin"}) {
		t.Errorf("required %v, want [origin entries cache_failing]", s.Required)
	}
	want := map[string]struct{ typ, format string }{
		"origin":        {"string", ""},
		"entries":       {"integer", ""},
		"age_seconds":   {"integer", ""},
		"list_time":     {"string", "date-time"},
		"last_success":  {"string", "date-time"},
		"last_error":    {"string", ""},
		"cache_failing": {"boolean", ""},
	}
	if len(s.Properties) != len(want) {
		t.Errorf("%d properties, want %d", len(s.Properties), len(want))
	}
	for k, w := range want {
		p := s.Properties[k]
		if p == nil || p.Value == nil {
			t.Errorf("property %s missing", k)
			continue
		}
		if !p.Value.Type.Is(w.typ) || p.Value.Format != w.format {
			t.Errorf("property %s: type %v format %q, want %s %q", k, p.Value.Type, p.Value.Format, w.typ, w.format)
		}
	}
	if o := s.Properties["origin"]; o != nil && o.Value != nil {
		var enum []string
		for _, e := range o.Value.Enum {
			enum = append(enum, e.(string))
		}
		if !reflect.DeepEqual(enum, []string{"none", "cache", "provider"}) {
			t.Errorf("origin enum %v, want [none cache provider]", enum)
		}
	}
	// The descriptions are the shared text, whole: a comma inside an unquoted
	// flow-style value would cut it short.
	for k, d := range map[string]string{
		"age_seconds":  "Age of the live list; absent when origin is none",
		"list_time":    "When the live list was last known good; absent when origin is none",
		"last_success": "Last good pull in this process; absent if none",
		"last_error":   "The latest pull error, made safe (printable, at most 200 characters); absent when the latest pull succeeded",
	} {
		if p := s.Properties[k]; p != nil && p.Value != nil && p.Value.Description != d {
			t.Errorf("property %s: description %q, want %q", k, p.Value.Description, d)
		}
	}

	stats := file.Components.Schemas["Stats"].Value
	pk := stats.Properties["personal_keys"]
	if pk == nil || pk.Ref != "#/components/schemas/PersonalKeysStatus" {
		t.Fatalf("Stats.personal_keys is not a PersonalKeysStatus: %+v", pk)
	}
	for _, r := range stats.Required {
		if r == "personal_keys" {
			t.Error("Stats.personal_keys must be optional")
		}
	}

	// The generated types: absent when nil, the optional fields omitted.
	b, err := json.Marshal(Stats{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["personal_keys"]; ok {
		t.Errorf("personal_keys present in an empty Stats: %s", b)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	age := 3
	st := PersonalKeysStatus{Origin: Provider, Entries: 2, AgeSeconds: &age, ListTime: &now,
		LastSuccess: &now, CacheFailing: false}
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	_ = json.Unmarshal(b, &v)
	if err := s.VisitJSON(v); err != nil {
		t.Errorf("%s does not match PersonalKeysStatus: %v", b, err)
	}
	b, _ = json.Marshal(PersonalKeysStatus{Origin: None})
	if string(b) != `{"cache_failing":false,"entries":0,"origin":"none"}` {
		t.Errorf("origin none: %s", b)
	}
}

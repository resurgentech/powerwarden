package powerwardenapi

import (
	"encoding/json"
	"os"
	"testing"
)

const fixtureDir = "../../specs/fixtures/keyrequest/"

// fixture is one recorded HTTP exchange from specs/fixtures/keyrequest.
type fixture struct {
	Status int              `json:"x-status"`
	Value  *json.RawMessage `json:"value"`
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	b, err := os.ReadFile(fixtureDir + name)
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return f
}

// TestKeyRequestDiscovery checks GET /v1/key-request against the discovery
// contract: no auth, 200 with KeyRequestDiscovery, 404 when not configured,
// and the fixtures agree with the schema.
func TestKeyRequestDiscovery(t *testing.T) {
	file, embedded := loadSpecs(t)
	item := file.Paths.Find("/v1/key-request")
	if item == nil || item.Get == nil {
		t.Fatal("GET /v1/key-request missing from the spec")
	}
	op := item.Get
	if op.OperationID != "getKeyRequestDiscovery" {
		t.Errorf("operationId %q", op.OperationID)
	}
	if op.Security == nil || len(*op.Security) != 0 {
		t.Errorf("security %v, want []", op.Security)
	}
	ok := op.Responses.Status(200)
	if ok == nil || ok.Value == nil {
		t.Fatal("no 200 response")
	}
	mt := ok.Value.Content.Get("application/json")
	if mt == nil || mt.Schema == nil || mt.Schema.Ref != "#/components/schemas/KeyRequestDiscovery" {
		t.Fatalf("200 schema is not KeyRequestDiscovery: %+v", mt)
	}
	if op.Responses.Status(404) == nil {
		t.Error("no 404 response")
	}
	if embedded.Components.Schemas["KeyRequestDiscovery"] == nil {
		t.Error("KeyRequestDiscovery missing from the embedded spec; run `just generate`")
	}

	ref := file.Components.Schemas["KeyRequestDiscovery"]
	if ref == nil || ref.Value == nil {
		t.Fatal("schema KeyRequestDiscovery missing")
	}
	s := ref.Value
	if len(s.Required) != 1 || s.Required[0] != "request_url" {
		t.Errorf("required %v, want [request_url]", s.Required)
	}
	u := s.Properties["request_url"]
	if u == nil || u.Value == nil || !u.Value.Type.Is("string") || u.Value.Format != "uri" {
		t.Errorf("request_url is not a string of format uri: %+v", u)
	}

	// The 200 fixture validates against the schema; the 404 fixture has no body.
	found := loadFixture(t, "discovery.json")
	if found.Status != 200 || found.Value == nil {
		t.Fatalf("discovery.json: status %d, value %v", found.Status, found.Value)
	}
	var v any
	if err := json.Unmarshal(*found.Value, &v); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitJSON(v); err != nil {
		t.Errorf("discovery.json does not match KeyRequestDiscovery: %v", err)
	}
	none := loadFixture(t, "discovery-none.json")
	if none.Status != 404 || none.Value != nil {
		t.Errorf("discovery-none.json: status %d, value %v", none.Status, none.Value)
	}
}

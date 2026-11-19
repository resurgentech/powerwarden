package powermasterapi

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const specFile = "../../specs/openapi/powermaster.yml"

// operations returns "METHOD path" -> operationId for every operation in doc.
func operations(doc *openapi3.T) map[string]string {
	ops := map[string]string{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			ops[method+" "+path] = op.OperationID
		}
	}
	return ops
}

func loadSpecs(t *testing.T) (file, embedded *openapi3.T) {
	t.Helper()
	file, err := openapi3.NewLoader().LoadFromFile(specFile)
	if err != nil {
		t.Fatalf("load %s: %v", specFile, err)
	}
	if err := file.Validate(context.Background()); err != nil {
		t.Fatalf("validate %s: %v", specFile, err)
	}
	embedded, err = GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	return file, embedded
}

// TestEmbeddedSpecCoversSpecFile checks that every path/operation in the spec
// file is present in the embedded (generated) spec, and vice versa.
func TestEmbeddedSpecCoversSpecFile(t *testing.T) {
	file, embedded := loadSpecs(t)
	want, got := operations(file), operations(embedded)
	if len(want) == 0 {
		t.Fatal("spec file has no operations")
	}
	for k, id := range want {
		gid, ok := got[k]
		if !ok {
			t.Errorf("operation %s (%s) missing from embedded spec; run `just generate`", k, id)
			continue
		}
		if goName(gid) != goName(id) {
			t.Errorf("operation %s: operationId %q in spec file, %q embedded", k, id, gid)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("embedded spec has stale operation %s; run `just generate`", k)
		}
	}
}

// TestClientHasEveryOperation checks that the generated client exposes one
// method per operationId.
func TestClientHasEveryOperation(t *testing.T) {
	file, _ := loadSpecs(t)
	iface := reflect.TypeOf((*ClientInterface)(nil)).Elem()
	var ids []string
	for k, id := range operations(file) {
		if id == "" {
			t.Errorf("operation %s has no operationId", k)
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		name := goName(id)
		if _, ok := iface.MethodByName(name); !ok {
			t.Errorf("ClientInterface has no method %s for operationId %s", name, id)
		}
	}
}

// unauthenticated lists the only operations with an empty security override.
var unauthenticated = map[string]bool{
	"GET /v1/healthz":     true,
	"GET /v1/key-request": true,
}

// TestHealthzIsUnauthenticated checks the global bearerAuth requirement and
// its empty override on /v1/healthz and /v1/key-request, and on nothing else.
func TestHealthzIsUnauthenticated(t *testing.T) {
	file, _ := loadSpecs(t)
	if len(file.Security) != 1 {
		t.Fatalf("want one global security requirement, got %v", file.Security)
	}
	if _, ok := file.Security[0]["bearerAuth"]; !ok {
		t.Fatalf("global security is not bearerAuth: %v", file.Security)
	}
	for k, op := range allOps(file) {
		empty := op.Security != nil && len(*op.Security) == 0
		if unauthenticated[k] != empty {
			t.Errorf("%s: security override %v (only /v1/healthz and /v1/key-request may be unauthenticated)", k, op.Security)
		}
	}
	for k := range unauthenticated {
		if _, ok := allOps(file)[k]; !ok {
			t.Errorf("unauthenticated operation %s missing from the spec", k)
		}
	}
}

// TestHostSchemasHaveNoSecretFields is the spec-side half of the host JSON
// denylist: no host or event schema may declare a hardware address or
// credential field.
func TestHostSchemasHaveNoSecretFields(t *testing.T) {
	file, _ := loadSpecs(t)
	deny := []string{"ip", "addr", "address", "user", "username", "password", "passwd",
		"cred", "credential", "secret", "key", "ssh_key", "token", "pdu_host", "outlet", "bmc"}
	for _, name := range []string{"Host", "HostDetail", "HostStatus", "HostEvent", "Event"} {
		ref, ok := file.Components.Schemas[name]
		if !ok {
			t.Fatalf("schema %s missing", name)
		}
		for _, prop := range propertyNames(ref.Value, map[*openapi3.Schema]bool{}) {
			for _, d := range deny {
				if prop == d || strings.HasPrefix(prop, d+"_") || strings.HasSuffix(prop, "_"+d) {
					t.Errorf("schema %s has forbidden property %q", name, prop)
				}
			}
		}
	}
}

// goName is oapi-codegen's Go name for an operationId (first letter upper);
// the embedded spec stores operationIds in this form.
func goName(id string) string {
	if id == "" {
		return id
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

func allOps(doc *openapi3.T) map[string]*openapi3.Operation {
	ops := map[string]*openapi3.Operation{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			ops[method+" "+path] = op
		}
	}
	return ops
}

// propertyNames walks a schema (properties, allOf/oneOf/anyOf, items) and
// returns every property name it declares.
func propertyNames(s *openapi3.Schema, seen map[*openapi3.Schema]bool) []string {
	if s == nil || seen[s] {
		return nil
	}
	seen[s] = true
	var out []string
	for name, p := range s.Properties {
		out = append(out, name)
		out = append(out, propertyNames(p.Value, seen)...)
	}
	for _, group := range []openapi3.SchemaRefs{s.AllOf, s.OneOf, s.AnyOf} {
		for _, r := range group {
			out = append(out, propertyNames(r.Value, seen)...)
		}
	}
	if s.Items != nil {
		out = append(out, propertyNames(s.Items.Value, seen)...)
	}
	return out
}

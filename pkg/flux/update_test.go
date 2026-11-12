package flux

import (
	"context"
	"fmt"
	"testing"

	resource "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/v1/hostconnections"
)

type updateFixture struct {
	reads, updates, conflicts int
	last                      resource.MutationRequest
	requests                  []resource.MutationRequest
}

func (f *updateFixture) Catalog(context.Context, string, resource.CatalogRequest) (resource.Catalog, error) {
	panic("unexpected catalog")
}
func (f *updateFixture) ResolveTarget(context.Context, string, hostconnections.ResolveTargetRequest) (hostconnections.ResolvedTarget, error) {
	panic("unexpected resolve")
}
func (f *updateFixture) ResolveSchema(context.Context, string, resource.SchemaRequest) (resource.SchemaBinding, error) {
	panic("unexpected schema resolution")
}
func (f *updateFixture) GetSchemaDocument(context.Context, string, resource.SchemaDocumentRequest) (resource.SchemaDocument, error) {
	panic("unexpected schema document")
}
func (f *updateFixture) Enumerate(context.Context, string, string, resource.EnumerateRequest) (resource.SourcePage, error) {
	panic("unexpected enumerate")
}
func (f *updateFixture) Get(context.Context, string, string, resource.GetRequest) (resource.Record, error) {
	f.reads++
	revision := fmt.Sprintf("rev-%d", f.reads)
	incarnation := fmt.Sprintf("uid-%d", f.reads)
	body, _ := resource.ValueFromNative(map[string]any{"spec": map[string]any{"unrelated": int64(f.reads), "suspend": false}})
	return resource.Record{Body: body, SourceRevision: &revision, Reference: resource.ResourceReference{Incarnation: &incarnation}}, nil
}
func (f *updateFixture) Update(_ context.Context, _ string, _ string, req resource.MutationRequest) (resource.Record, error) {
	f.updates++
	f.last = req
	f.requests = append(f.requests, req)
	if f.updates <= f.conflicts {
		return resource.Record{}, resource.NewError("CONFLICT", "The object changed during the update.")
	}
	return resource.Record{Body: req.Input}, nil
}
func TestUpdateRetriesFreshObjects(t *testing.T) {
	fixture := &updateFixture{conflicts: 2}
	a := access{client: fixture}
	record, err := a.update(context.Background(), hostconnections.ResolvedTarget{}, resource.ResourceType{IdentityDimensions: []resource.Dimension{{Name: "namespace"}}}, "ns", "name", func(m map[string]any) error { objectMap(m["spec"])["suspend"] = true; return nil })
	if err != nil || fixture.reads != 3 || fixture.updates != 3 || fixture.last.ExpectedRevision == nil {
		t.Fatalf("reads=%d updates=%d err=%v", fixture.reads, fixture.updates, err)
	}
	for i, req := range fixture.requests {
		if req.ExpectedRevision == nil || *req.ExpectedRevision != fmt.Sprintf("rev-%d", i+1) || req.Reference.Incarnation == nil || *req.Reference.Incarnation != fmt.Sprintf("uid-%d", i+1) {
			t.Fatalf("stale update %d: %+v", i, req)
		}
	}
	object, err := nativeObject(record)
	if err != nil {
		t.Fatal(err)
	}
	spec := objectMap(object["spec"])
	if spec["unrelated"] != int64(3) || spec["suspend"] != true {
		t.Fatalf("lost fresh fields: %v", spec)
	}
}
func TestUpdateStopsAfterThreeConflicts(t *testing.T) {
	fixture := &updateFixture{conflicts: 4}
	a := access{client: fixture}
	_, err := a.update(context.Background(), hostconnections.ResolvedTarget{}, resource.ResourceType{IdentityDimensions: []resource.Dimension{{Name: "namespace"}}}, "ns", "name", func(map[string]any) error { return nil })
	if resource.ErrorCode(err) != "CONFLICT" || fixture.updates != 3 {
		t.Fatalf("updates=%d err=%v", fixture.updates, err)
	}
}

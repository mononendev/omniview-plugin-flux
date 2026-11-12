package flux

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	resource "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/v1/hostconnections"
)

type graphFixture struct {
	updateFixture
	objects   map[string]map[string]any
	events    []string
	schemaErr error
}

func (f *graphFixture) ResolveTarget(_ context.Context, _ string, req hostconnections.ResolveTargetRequest) (hostconnections.ResolvedTarget, error) {
	return hostconnections.ResolvedTarget{TargetID: req.Type, Target: resource.ReadTarget{Route: req.Route, Provider: "kubernetes", Authority: "host", Type: req.Type, Scope: req.Scope, Representation: req.Representation}}, nil
}
func (f *graphFixture) ResolveSchema(_ context.Context, requestedOwner string, req resource.SchemaRequest) (resource.SchemaBinding, error) {
	if f.schemaErr != nil {
		return resource.SchemaBinding{}, f.schemaErr
	}
	if requestedOwner != owner || req.Target.Representation == nil || req.Target.Auxiliary != nil || *req.Target.Representation != (resource.Representation{ID: "v1", Revision: "format-1"}) {
		return resource.SchemaBinding{}, unsupported()
	}
	for key := range f.objects {
		if strings.HasPrefix(key, req.Type+"/") {
			return resource.SchemaBinding{State: "available", Dialect: "https://json-schema.org/draft/2020-12/schema", Document: json.RawMessage(`{"properties":{"status":{"properties":{"lastHandledReconcileAt":{"type":"string"},"conditions":{"type":"array"}}}}}`)}, nil
		}
	}
	return resource.SchemaBinding{}, unsupported()
}
func (f *graphFixture) Get(_ context.Context, _ string, id string, req resource.GetRequest) (resource.Record, error) {
	key := id + "/" + req.Reference.Address.ID
	f.events = append(f.events, "get:"+key)
	object, ok := f.objects[key]
	if !ok {
		return resource.Record{}, fmt.Errorf("missing object")
	}
	body, err := resource.ValueFromNative(object)
	revision := "revision"
	return resource.Record{Body: body, Reference: req.Reference, SourceRevision: &revision}, err
}
func (f *graphFixture) Update(_ context.Context, _ string, id string, req resource.MutationRequest) (resource.Record, error) {
	key := id + "/" + req.Reference.Address.ID
	f.events = append(f.events, "update:"+key)
	native, err := resource.NativeFromValue(req.Input)
	if err != nil {
		return resource.Record{}, err
	}
	object := native.(map[string]any)
	stamp := stringField(objectMap(objectMap(object["metadata"])["annotations"]), "reconcile.fluxcd.io/requestedAt")
	object["status"] = map[string]any{"lastHandledReconcileAt": stamp, "observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	f.objects[key] = object
	body, err := resource.ValueFromNative(object)
	return resource.Record{Body: body, Reference: req.Reference}, err
}
func graphDescriptor(kind, group, plural string) resource.ResourceType {
	return resource.ResourceType{Type: "k8s:" + group + ".toolkit.fluxcd.io/" + plural, DisplayName: kind, IdentityDimensions: []resource.Dimension{{Name: "namespace"}}, QueryDimensions: []resource.Dimension{{Name: "namespace"}}, DefaultRepresentation: "v1", Representations: []resource.Representation{{ID: "v1", Revision: "format-1"}}}
}
func graphObject(spec map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"generation": int64(1)}, "spec": spec}
}
func TestGraphWaitsForSourceEvenWhenParentDoesNotWait(t *testing.T) {
	parent := graphDescriptor("Kustomization", "kustomize", "kustomizations")
	source := graphDescriptor("GitRepository", "source", "gitrepositories")
	f := &graphFixture{objects: map[string]map[string]any{parent.Type + "/app": graphObject(map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "src"}}), source.Type + "/src": graphObject(map[string]any{})}}
	a := access{client: f, catalog: resource.Catalog{Types: []resource.ResourceType{parent, source}}}
	if _, err := a.reconcileGraph(context.Background(), objectKey{"Kustomization", "ns", "app"}, "stamp", false, true, map[objectKey]bool{}); err != nil {
		t.Fatal(err)
	}
	events := strings.Join(f.events, ",")
	sourceUpdate := strings.Index(events, "update:"+source.Type+"/src")
	sourceRead := strings.LastIndex(events, "get:"+source.Type+"/src")
	parentUpdate := strings.Index(events, "update:"+parent.Type+"/app")
	if sourceUpdate < 0 || sourceRead < sourceUpdate || parentUpdate < sourceRead {
		t.Fatalf("source not awaited: %s", events)
	}
}
func TestGraphSkipsStaticOCIAndRejectsSuspendedSource(t *testing.T) {
	chart := graphDescriptor("HelmChart", "source", "helmcharts")
	repo := graphDescriptor("HelmRepository", "source", "helmrepositories")
	f := &graphFixture{objects: map[string]map[string]any{chart.Type + "/chart": graphObject(map[string]any{"sourceRef": map[string]any{"kind": "HelmRepository", "name": "repo"}}), repo.Type + "/repo": graphObject(map[string]any{"type": "oci"})}}
	a := access{client: f, catalog: resource.Catalog{Types: []resource.ResourceType{chart, repo}}}
	if _, err := a.reconcileGraph(context.Background(), objectKey{"HelmChart", "ns", "chart"}, "stamp", false, true, map[objectKey]bool{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.events, ","), "update:"+repo.Type) {
		t.Fatalf("static source updated: %v", f.events)
	}
	f.events = nil
	f.objects[repo.Type+"/repo"] = graphObject(map[string]any{"suspend": true})
	if _, err := a.reconcileGraph(context.Background(), objectKey{"HelmChart", "ns", "chart"}, "stamp", false, true, map[objectKey]bool{}); err == nil {
		t.Fatal("suspended source accepted")
	}
	if strings.Contains(strings.Join(f.events, ","), "update:") {
		t.Fatalf("write after suspended source: %v", f.events)
	}
}
func TestGraphRejectsVisitedCycleBeforeDispatch(t *testing.T) {
	f := &graphFixture{}
	a := access{client: f}
	key := objectKey{"Kustomization", "ns", "app"}
	if _, err := a.reconcileGraph(context.Background(), key, "stamp", false, true, map[objectKey]bool{{"kustomization", "ns", "app"}: true}); err == nil || len(f.events) != 0 {
		t.Fatalf("cycle dispatched: %v, %v", err, f.events)
	}
}

func TestGraphSchemaDenialPreventsWrites(t *testing.T) {
	typ := graphDescriptor("Kustomization", "kustomize", "kustomizations")
	denied := fmt.Errorf("schema denied")
	f := &graphFixture{schemaErr: denied, objects: map[string]map[string]any{typ.Type + "/app": graphObject(map[string]any{})}}
	a := access{client: f, catalog: resource.Catalog{Types: []resource.ResourceType{typ}}}
	if _, err := a.reconcileGraph(context.Background(), objectKey{"Kustomization", "ns", "app"}, "stamp", false, false, map[objectKey]bool{}); err != denied {
		t.Fatalf("schema denial lost: %v", err)
	}
	if strings.Contains(strings.Join(f.events, ","), "update:") {
		t.Fatalf("write after schema denial: %v", f.events)
	}
}

func TestWaitTerminalFailureIsExecutionFailure(t *testing.T) {
	typ := graphDescriptor("GitRepository", "source", "gitrepositories")
	object := graphObject(map[string]any{})
	object["status"] = map[string]any{"lastHandledReconcileAt": "stamp", "observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	f := &graphFixture{objects: map[string]map[string]any{typ.Type + "/src": object}}
	a := access{client: f}
	target := hostconnections.ResolvedTarget{TargetID: typ.Type, Target: resource.ReadTarget{Type: typ.Type}}
	_, err := a.waitReady(context.Background(), target, typ, "ns", "src", "stamp")
	if err == nil || resource.ErrorCode(err) != "INTERNAL" {
		t.Fatalf("terminal failure classification: %v", err)
	}
}

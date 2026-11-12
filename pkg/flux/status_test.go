package flux

import (
	"context"
	"strconv"
	"testing"

	resource "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/v1/hostconnections"
)

type pagesFixture struct {
	updateFixture
	pages []resource.SourcePage
	calls int
	scope resource.QueryScope
}

func (f *pagesFixture) ResolveTarget(_ context.Context, _ string, req hostconnections.ResolveTargetRequest) (hostconnections.ResolvedTarget, error) {
	f.scope = req.Scope
	return hostconnections.ResolvedTarget{TargetID: "target", Target: resource.ReadTarget{Route: req.Route, Type: req.Type, Scope: req.Scope, Representation: req.Representation}}, nil
}
func (f *pagesFixture) Enumerate(context.Context, string, string, resource.EnumerateRequest) (resource.SourcePage, error) {
	page := f.pages[f.calls]
	f.calls++
	return page, nil
}
func testCatalog() resource.Catalog {
	return resource.Catalog{Types: []resource.ResourceType{{
		Type: "k8s:source.toolkit.fluxcd.io/gitrepositories", DisplayName: "GitRepository",
		IdentityDimensions: []resource.Dimension{{Name: "namespace"}}, QueryDimensions: []resource.Dimension{{Name: "namespace"}},
		DefaultRepresentation: "v1", Representations: []resource.Representation{{ID: "v1", Revision: "format-1"}},
		Capabilities: []resource.Capability{{Operation: resource.OperationEnumerate, Scope: resource.QueryScope{{Dimension: "namespace", Match: resource.ScopeAny}}}},
	}}}
}
func TestStatusContinuesEmptyPagesAndPreservesCoverage(t *testing.T) {
	cursor := "next"
	f := &pagesFixture{pages: []resource.SourcePage{
		{Records: []resource.Record{}, NextCursor: &cursor, Coverage: resource.Coverage{Kind: "partial", Reason: "limited"}},
		{Records: []resource.Record{}, Coverage: resource.Coverage{Kind: "unknown", Reason: "unknown"}},
	}}
	a := access{client: f, catalog: testCatalog()}
	result, err := a.status(context.Background(), "", []string{"GitRepository"})
	if err != nil || f.calls != 2 || len(result.Coverage) != 1 || len(result.Coverage[0].Pages) != 2 || result.Coverage[0].Pages[0].Kind != "partial" || result.Coverage[0].Pages[1].Kind != "unknown" {
		t.Fatalf("result=%+v calls=%d err=%v", result, f.calls, err)
	}
}
func TestStaticObjectsWithoutConditionsAreNotFindings(t *testing.T) {
	f, ok := findingFor("HelmRepository", map[string]any{"spec": map[string]any{"type": "oci"}})
	if ok || f.Ready != nil {
		t.Fatal("static object incorrectly reported")
	}
	_, ok = findingFor("HelmRepository", map[string]any{"spec": map[string]any{"type": "oci", "suspend": true}})
	if !ok {
		t.Fatal("suspended static object omitted")
	}
}

func TestStatusRejectsRepeatedCursor(t *testing.T) {
	cursor := "same"
	f := &pagesFixture{pages: []resource.SourcePage{
		{NextCursor: &cursor},
		{NextCursor: &cursor},
	}}
	a := access{client: f, catalog: testCatalog()}
	_, err := a.status(context.Background(), "", []string{"GitRepository"})
	if err == nil || f.calls != 2 {
		t.Fatalf("calls=%d err=%v", f.calls, err)
	}
}

func TestStatusStopsAtPageLimit(t *testing.T) {
	f := &pagesFixture{}
	for i := range 10 {
		cursor := strconv.Itoa(i)
		f.pages = append(f.pages, resource.SourcePage{NextCursor: &cursor})
	}
	a := access{client: f, catalog: testCatalog()}
	result, err := a.status(context.Background(), "", []string{"GitRepository"})
	if err != nil || f.calls != 10 || !result.Truncated || len(result.TruncatedKinds) != 1 || result.Coverage[0].Stopped != "page limit" {
		t.Fatalf("result=%+v calls=%d err=%v", result, f.calls, err)
	}
}

func TestStatusUsesAdvertisedScope(t *testing.T) {
	values := []string{"team-a"}
	catalog := testCatalog()
	catalog.Types[0].Capabilities[0].Scope[0] = resource.ScopeConstraint{Dimension: "namespace", Match: resource.ScopeOneOf, Values: &values}
	f := &pagesFixture{pages: []resource.SourcePage{{}}}
	a := access{client: f, catalog: catalog}
	if _, err := a.status(context.Background(), "", []string{"GitRepository"}); err != nil {
		t.Fatal(err)
	}
	if f.scope[0].Match != resource.ScopeOneOf || f.scope[0].Values == nil || (*f.scope[0].Values)[0] != "team-a" {
		t.Fatalf("scope widened: %+v", f.scope)
	}
	if _, err := a.status(context.Background(), "team-b", []string{"GitRepository"}); resource.ErrorCode(err) != "UNSUPPORTED" {
		t.Fatalf("scope denial: %v", err)
	}
	catalog.Types[0].Capabilities = nil
	a.catalog = catalog
	if _, err := a.status(context.Background(), "", []string{"GitRepository"}); resource.ErrorCode(err) != "UNSUPPORTED" {
		t.Fatalf("missing enumerate: %v", err)
	}
}

func TestStatusMissingDefaultAndExplicitKind(t *testing.T) {
	a := access{client: &pagesFixture{}, catalog: resource.Catalog{}}
	result, err := a.status(context.Background(), "", nil)
	if err != nil || len(result.Findings) != 0 {
		t.Fatalf("default result=%+v err=%v", result, err)
	}
	if _, err := a.status(context.Background(), "", []string{"GitRepository"}); resource.ErrorCode(err) != "UNSUPPORTED" {
		t.Fatalf("explicit missing: %v", err)
	}
}

func TestStatusFindingBound(t *testing.T) {
	body, err := resource.ValueFromNative(map[string]any{"metadata": map[string]any{"generation": int64(1)}, "status": map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &pagesFixture{}
	for i := range 6 {
		page := resource.SourcePage{}
		for range 100 {
			page.Records = append(page.Records, resource.Record{Body: body})
		}
		cursor := strconv.Itoa(i)
		page.NextCursor = &cursor
		f.pages = append(f.pages, page)
	}
	a := access{client: f, catalog: testCatalog()}
	result, err := a.status(context.Background(), "", []string{"GitRepository"})
	if err != nil || len(result.Findings) != 500 || !result.Truncated || result.Coverage[0].Stopped != "overall limit" {
		t.Fatalf("findings=%d result=%+v err=%v", len(result.Findings), result, err)
	}
}

func TestStatusRejectsStaleReady(t *testing.T) {
	body := map[string]any{"metadata": map[string]any{"generation": int64(2)}, "status": map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
	f, include := findingFor("GitRepository", body)
	if !include || f.Ready != nil {
		t.Fatalf("stale readiness: %+v", f)
	}
}

func TestStatusExaminedBound(t *testing.T) {
	body, err := resource.ValueFromNative(map[string]any{"metadata": map[string]any{"generation": int64(1)}, "status": map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &pagesFixture{}
	for i := range 20 {
		page := resource.SourcePage{}
		for range 100 {
			page.Records = append(page.Records, resource.Record{Body: body})
		}
		if i%10 != 9 {
			cursor := strconv.Itoa(i)
			page.NextCursor = &cursor
		}
		f.pages = append(f.pages, page)
	}
	catalog := testCatalog()
	for _, pair := range [][2]string{{"OCIRepository", "ocirepositories"}, {"Bucket", "buckets"}} {
		typ := catalog.Types[0]
		typ.DisplayName = pair[0]
		typ.Type = "k8s:source.toolkit.fluxcd.io/" + pair[1]
		catalog.Types = append(catalog.Types, typ)
	}
	a := access{client: f, catalog: catalog}
	result, err := a.status(context.Background(), "", []string{"GitRepository", "OCIRepository", "Bucket"})
	if err != nil || f.calls != 20 || !result.Truncated || len(result.TruncatedKinds) != 1 || result.TruncatedKinds[0] != "Bucket" || len(result.Findings) != 0 {
		t.Fatalf("calls=%d result=%+v err=%v", f.calls, result, err)
	}
}

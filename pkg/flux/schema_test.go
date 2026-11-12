package flux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	resource "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
)

func TestSchemaNodeLocalReferences(t *testing.T) {
	doc := json.RawMessage(`{"definitions":{"object":{"properties":{"spec":{"$ref":"#/definitions/spec"}}},"spec":{"properties":{"suspend":{"type":"boolean"}}}},"root":{"$ref":"#/definitions/object"}}`)
	node, err := schemaNode(doc, "#/root", "spec", "suspend")
	if err != nil || node["type"] != "boolean" {
		t.Fatalf("node=%v err=%v", node, err)
	}
}

func TestSchemaNodeRejectsUnsafeReferences(t *testing.T) {
	for _, doc := range []string{
		`{"$ref":"https://example.com/schema"}`,
		`{"$ref":"#/definitions/a","definitions":{"a":{"$ref":"#/definitions/a"}}}`,
	} {
		if _, err := schemaNode(json.RawMessage(doc), ""); err == nil {
			t.Fatalf("accepted %s", doc)
		}
	}
}

func TestSchemaNodeReferenceHopLimit(t *testing.T) {
	for _, count := range []int{64, 65} {
		definitions := make(map[string]any)
		for i := range count - 1 {
			definitions[fmt.Sprint(i)] = map[string]any{"$ref": fmt.Sprintf("#/definitions/%d", i+1)}
		}
		definitions[fmt.Sprint(count-1)] = map[string]any{"type": "boolean"}
		document, err := json.Marshal(map[string]any{"definitions": definitions, "$ref": "#/definitions/0"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = schemaNode(document, "")
		if (err == nil) != (count == 64) {
			t.Fatalf("%d hops: %v", count, err)
		}
	}
}

var errSchemaTargetDenied = errors.New("schema target denied")

type schemaFixture struct {
	updateFixture
	route          resource.AccessRoute
	typ            string
	representation resource.Representation
	binding        resource.SchemaBinding
	document       resource.SchemaDocument
	resolveErr     error
	documentErr    error
	resolutions    int
	documents      int
}

func newSchemaFixture() *schemaFixture {
	reference := resource.SchemaDocumentReference{ID: "flux-schema", Revision: "document-3"}
	const dialect = "https://json-schema.org/draft/2020-12/schema"
	return &schemaFixture{
		route:          resource.AccessRoute{Connection: "cluster-a"},
		typ:            "k8s:kustomize.toolkit.fluxcd.io/kustomizations",
		representation: resource.Representation{ID: "v1", Revision: "format-7"},
		binding:        resource.SchemaBinding{State: "available", Dialect: dialect, Root: "#/definitions/object", DocumentRef: &reference},
		document: resource.SchemaDocument{
			Reference: reference,
			Dialect:   dialect,
			Document:  json.RawMessage(`{"properties":{"spec":{"properties":{"suspend":{"type":"string"}}}},"definitions":{"object":{"properties":{"spec":{"$ref":"#/definitions/spec"}}},"spec":{"properties":{"suspend":{"type":"boolean"}}}}}`),
		},
	}
}

func (f *schemaFixture) admits(requestedOwner string, req resource.SchemaRequest) bool {
	return requestedOwner == owner && req.Route == f.route && req.Type == f.typ &&
		req.Target.Auxiliary == nil && req.Target.Representation != nil && *req.Target.Representation == f.representation
}

func (f *schemaFixture) ResolveSchema(_ context.Context, requestedOwner string, req resource.SchemaRequest) (resource.SchemaBinding, error) {
	f.resolutions++
	if !f.admits(requestedOwner, req) {
		return resource.SchemaBinding{}, errSchemaTargetDenied
	}
	return f.binding, f.resolveErr
}

func (f *schemaFixture) GetSchemaDocument(_ context.Context, requestedOwner string, req resource.SchemaDocumentRequest) (resource.SchemaDocument, error) {
	f.documents++
	if !f.admits(requestedOwner, req.Request) || f.binding.DocumentRef == nil || req.Reference != *f.binding.DocumentRef {
		return resource.SchemaDocument{}, errSchemaTargetDenied
	}
	return f.document, f.documentErr
}

func TestSchemaUsesCatalogSelectionAndRejectsOtherTargets(t *testing.T) {
	f := newSchemaFixture()
	a := access{client: f, route: f.route, catalog: resource.Catalog{Types: []resource.ResourceType{{
		Type: f.typ, DisplayName: "Kustomization", DefaultRepresentation: f.representation.ID,
		Representations: []resource.Representation{{ID: "other", Revision: "format-1"}, f.representation},
	}}}}
	typ, rep, err := a.descriptor("Kustomization")
	if err != nil {
		t.Fatal(err)
	}
	node, err := a.schema(context.Background(), typ, rep, "spec", "suspend")
	if err != nil || node["type"] != "boolean" || f.resolutions != 1 || f.documents != 1 {
		t.Fatalf("node=%v resolutions=%d documents=%d err=%v", node, f.resolutions, f.documents, err)
	}
	for _, tc := range []struct {
		name  string
		route resource.AccessRoute
		typ   string
		rep   resource.Representation
	}{
		{"connection", resource.AccessRoute{Connection: "cluster-b"}, f.typ, rep},
		{"type", f.route, "k8s:source.toolkit.fluxcd.io/gitrepositories", rep},
		{"representation", f.route, f.typ, resource.Representation{ID: "other", Revision: rep.Revision}},
		{"revision", f.route, f.typ, resource.Representation{ID: rep.ID, Revision: "format-8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := access{client: f, route: tc.route}
			node, err := other.schema(context.Background(), resource.ResourceType{Type: tc.typ}, tc.rep, "spec", "suspend")
			if node != nil || !errors.Is(err, errSchemaTargetDenied) || f.documents != 1 {
				t.Fatalf("schema escaped target: node=%v documents=%d err=%v", node, f.documents, err)
			}
		})
	}
}

func TestSchemaInlineBindingUsesLocalRoot(t *testing.T) {
	f := newSchemaFixture()
	f.binding.DocumentRef = nil
	f.binding.Document = f.document.Document
	a := access{client: f, route: f.route}
	node, err := a.schema(context.Background(), resource.ResourceType{Type: f.typ}, f.representation, "spec", "suspend")
	if err != nil || node["type"] != "boolean" || f.resolutions != 1 || f.documents != 0 {
		t.Fatalf("node=%v resolutions=%d documents=%d err=%v", node, f.resolutions, f.documents, err)
	}
}

func TestSchemaRejectsDeniedOrUnassociatedDocuments(t *testing.T) {
	denied := errors.New("schema operation denied")
	for _, tc := range []struct {
		name      string
		change    func(*schemaFixture)
		wantError error
		documents int
	}{
		{"resolve denied", func(f *schemaFixture) { f.resolveErr = denied }, denied, 0},
		{"document denied", func(f *schemaFixture) { f.documentErr = denied }, denied, 1},
		{"absent", func(f *schemaFixture) { f.binding = resource.SchemaBinding{State: "absent"} }, nil, 0},
		{"unavailable", func(f *schemaFixture) { f.binding = resource.SchemaBinding{State: "unavailable"} }, nil, 0},
		{"other document", func(f *schemaFixture) { f.document.Reference.ID = "other" }, nil, 1},
		{"other document revision", func(f *schemaFixture) { f.document.Reference.Revision = "document-4" }, nil, 1},
		{"other dialect", func(f *schemaFixture) { f.document.Dialect = "https://json-schema.org/draft-07/schema" }, nil, 1},
		{"invalid JSON", func(f *schemaFixture) { f.document.Document = json.RawMessage(`{`) }, nil, 1},
		{"external root", func(f *schemaFixture) { f.binding.Root = "https://example.com/schema" }, nil, 1},
		{"missing root", func(f *schemaFixture) { f.binding.Root = "#/missing" }, nil, 1},
		{"external reference", func(f *schemaFixture) {
			f.document.Document = json.RawMessage(`{"definitions":{"object":{"$ref":"https://example.com/schema"}}}`)
		}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchemaFixture()
			tc.change(f)
			a := access{client: f, route: f.route}
			node, err := a.schema(context.Background(), resource.ResourceType{Type: f.typ}, f.representation, "spec", "suspend")
			if node != nil || err == nil || f.resolutions != 1 || f.documents != tc.documents {
				t.Fatalf("node=%v resolutions=%d documents=%d err=%v", node, f.resolutions, f.documents, err)
			}
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("denial lost: %v", err)
				}
			} else if resource.ErrorCode(err) != "UNSUPPORTED" {
				t.Fatalf("schema failure classification: %v", err)
			}
		})
	}
}

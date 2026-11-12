package flux

import (
	"context"
	"encoding/json"
	"strings"

	resource "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/v1/hostconnections"
)

const owner = "kubernetes"

var groups = map[string]string{
	"kustomization": "kustomize", "helmrelease": "helm",
	"gitrepository": "source", "ocirepository": "source", "helmrepository": "source", "helmchart": "source", "bucket": "source",
	"alert": "notification", "provider": "notification", "receiver": "notification",
	"imagerepository": "image", "imagepolicy": "image", "imageupdateautomation": "image",
}

func unsupported() error { return resource.NewError("UNSUPPORTED", "The operation is not supported.") }

type access struct {
	client  hostconnections.Client
	route   resource.AccessRoute
	catalog resource.Catalog
}

func (a *access) hasKind(kind string) bool {
	group := groups[strings.ToLower(kind)]
	prefix := "k8s:" + group + ".toolkit.fluxcd.io/"
	for _, typ := range a.catalog.Types {
		if strings.HasPrefix(typ.Type, prefix) && strings.EqualFold(typ.DisplayName, kind) {
			return true
		}
	}
	return false
}

func openAccess(ctx context.Context, client hostconnections.Client, connection string) (*access, error) {
	route := resource.AccessRoute{Connection: connection}
	catalog, err := client.Catalog(ctx, owner, resource.CatalogRequest{Route: route})
	if err != nil {
		return nil, err
	}
	return &access{client: client, route: route, catalog: catalog}, nil
}

func (a *access) descriptor(kind string) (resource.ResourceType, resource.Representation, error) {
	group, ok := groups[strings.ToLower(kind)]
	if !ok {
		return resource.ResourceType{}, resource.Representation{}, unsupported()
	}
	prefix := "k8s:" + group + ".toolkit.fluxcd.io/"
	var result resource.ResourceType
	found := false
	for _, typ := range a.catalog.Types {
		if strings.HasPrefix(typ.Type, prefix) && strings.EqualFold(typ.DisplayName, kind) {
			if found {
				return resource.ResourceType{}, resource.Representation{}, unsupported()
			}
			result, found = typ, true
		}
	}
	if !found {
		return result, resource.Representation{}, unsupported()
	}
	for _, rep := range result.Representations {
		if rep.ID == result.DefaultRepresentation {
			return result, rep, nil
		}
	}
	return result, resource.Representation{}, unsupported()
}

func (a *access) target(ctx context.Context, kind, namespace string) (hostconnections.ResolvedTarget, resource.ResourceType, resource.Representation, error) {
	typ, rep, err := a.descriptor(kind)
	if err != nil {
		return hostconnections.ResolvedTarget{}, typ, rep, err
	}
	if len(typ.QueryDimensions) != 1 || typ.QueryDimensions[0].Name != "namespace" || len(typ.IdentityDimensions) != 1 || typ.IdentityDimensions[0].Name != "namespace" {
		return hostconnections.ResolvedTarget{}, typ, rep, unsupported()
	}
	scope := make(resource.QueryScope, 0, len(typ.QueryDimensions))
	for _, dimension := range typ.QueryDimensions {
		if dimension.Name != "namespace" {
			return hostconnections.ResolvedTarget{}, typ, rep, unsupported()
		}
		constraint := resource.ScopeConstraint{Dimension: dimension.Name, Match: resource.ScopeAny}
		if namespace != "" {
			values := []string{namespace}
			constraint.Match = resource.ScopeOneOf
			constraint.Values = &values
		}
		scope = append(scope, constraint)
	}
	target, err := a.client.ResolveTarget(ctx, owner, hostconnections.ResolveTargetRequest{Route: a.route, Type: typ.Type, Scope: scope, Representation: rep.ID})
	return target, typ, rep, err
}

// enumerationTarget preserves the provider's advertised enumeration boundary.
func (a *access) enumerationTarget(ctx context.Context, kind, namespace string) (hostconnections.ResolvedTarget, error) {
	typ, rep, err := a.descriptor(kind)
	if err != nil {
		return hostconnections.ResolvedTarget{}, err
	}
	if len(typ.QueryDimensions) != 1 || typ.QueryDimensions[0].Name != "namespace" || len(typ.IdentityDimensions) != 1 || typ.IdentityDimensions[0].Name != "namespace" {
		return hostconnections.ResolvedTarget{}, unsupported()
	}
	var selected resource.QueryScope
	for _, capability := range typ.Capabilities {
		if capability.Operation != resource.OperationEnumerate || len(capability.Scope) != 1 {
			continue
		}
		constraint := capability.Scope[0]
		if constraint.Dimension != "namespace" {
			continue
		}
		allowed := constraint.Match == resource.ScopeAny
		if constraint.Match == resource.ScopeOneOf && constraint.Values != nil {
			if namespace == "" {
				allowed = true
			} else {
				for _, value := range *constraint.Values {
					if value == namespace {
						allowed = true
						break
					}
				}
			}
		}
		if !allowed {
			continue
		}
		if namespace != "" {
			values := []string{namespace}
			selected = resource.QueryScope{{Dimension: "namespace", Match: resource.ScopeOneOf, Values: &values}}
			break
		}
		if selected != nil {
			return hostconnections.ResolvedTarget{}, unsupported()
		}
		selected = capability.Scope
	}
	if selected == nil {
		return hostconnections.ResolvedTarget{}, unsupported()
	}
	return a.client.ResolveTarget(ctx, owner, hostconnections.ResolveTargetRequest{Route: a.route, Type: typ.Type, Scope: selected, Representation: rep.ID})
}

func (a *access) get(ctx context.Context, target hostconnections.ResolvedTarget, typ resource.ResourceType, namespace, name string) (resource.Record, error) {
	if len(typ.IdentityDimensions) != 1 || typ.IdentityDimensions[0].Name != "namespace" {
		return resource.Record{}, unsupported()
	}
	scope := make([]resource.ScopeEntry, 0, len(typ.IdentityDimensions))
	for _, dim := range typ.IdentityDimensions {
		if dim.Name != "namespace" || namespace == "" {
			return resource.Record{}, unsupported()
		}
		scope = append(scope, resource.ScopeEntry{Dimension: dim.Name, Value: namespace})
	}
	return a.client.Get(ctx, owner, target.TargetID, resource.GetRequest{Route: a.route, Representation: target.Target.Representation, Reference: resource.ResourceReference{Address: resource.ResourceAddress{Provider: target.Target.Provider, Authority: target.Target.Authority, Type: target.Target.Type, Scope: scope, ID: name}}})
}

func (a *access) schema(ctx context.Context, typ resource.ResourceType, rep resource.Representation, fields ...string) (map[string]any, error) {
	request := resource.SchemaRequest{Route: a.route, Type: typ.Type, Target: resource.SchemaTarget{Representation: &rep}}
	binding, err := a.client.ResolveSchema(ctx, owner, request)
	if err != nil {
		return nil, err
	}
	if binding.State != "available" {
		return nil, unsupported()
	}
	document := binding.Document
	if binding.DocumentRef != nil {
		referenced, err := a.client.GetSchemaDocument(ctx, owner, resource.SchemaDocumentRequest{Request: request, Reference: *binding.DocumentRef})
		if err != nil {
			return nil, err
		}
		if referenced.Reference != *binding.DocumentRef || referenced.Dialect != binding.Dialect {
			return nil, unsupported()
		}
		document = referenced.Document
	}
	if !json.Valid(document) {
		return nil, unsupported()
	}
	node, err := schemaNode(document, binding.Root, fields...)
	if err != nil {
		return nil, unsupported()
	}
	return node, nil
}

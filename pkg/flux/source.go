package flux

import (
	"context"
	"strings"

	resource "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
)

type objectKey struct{ Kind, Namespace, Name string }

// sourceOf follows Flux controller references without consulting unrelated
// objects such as Secrets or ConfigMaps.
func sourceOf(key objectKey, object map[string]any) (objectKey, bool, error) {
	spec := objectMap(object["spec"])
	var ref map[string]any
	switch strings.ToLower(key.Kind) {
	case "kustomization", "helmchart":
		ref = objectMap(spec["sourceRef"])
	case "helmrelease":
		ref = objectMap(spec["chartRef"])
		if ref == nil {
			chart := objectMap(spec["chart"])
			if chart == nil {
				return objectKey{}, false, unsupported()
			}
			kind := stringField(chart, "kind")
			if kind == "" {
				kind = "HelmChart"
			}
			namespace := key.Namespace
			if strings.EqualFold(kind, "HelmChart") {
				if ns := stringField(objectMap(objectMap(chart["spec"])["sourceRef"]), "namespace"); ns != "" {
					namespace = ns
				}
			}
			if !strings.EqualFold(kind, "HelmChart") && !strings.EqualFold(kind, "OCIRepository") {
				return objectKey{}, false, unsupported()
			}
			return objectKey{kind, namespace, key.Namespace + "-" + key.Name}, true, nil
		}
	default:
		return objectKey{}, false, nil
	}
	if ref == nil {
		return objectKey{}, false, unsupported()
	}
	kind, name := stringField(ref, "kind"), stringField(ref, "name")
	if strings.EqualFold(key.Kind, "HelmRelease") && kind == "" {
		kind = "HelmChart"
	}
	allowed := false
	switch strings.ToLower(key.Kind) {
	case "kustomization":
		allowed = strings.EqualFold(kind, "GitRepository") || strings.EqualFold(kind, "OCIRepository") || strings.EqualFold(kind, "Bucket")
	case "helmrelease":
		allowed = strings.EqualFold(kind, "HelmChart") || strings.EqualFold(kind, "OCIRepository")
	case "helmchart":
		allowed = strings.EqualFold(kind, "HelmRepository") || strings.EqualFold(kind, "GitRepository") || strings.EqualFold(kind, "Bucket")
	}
	if !allowed || name == "" {
		return objectKey{}, false, unsupported()
	}
	namespace := key.Namespace
	if !strings.EqualFold(key.Kind, "HelmChart") {
		if ns := stringField(ref, "namespace"); ns != "" {
			namespace = ns
		}
	}
	return objectKey{kind, namespace, name}, true, nil
}

func staticObject(key objectKey, object map[string]any) bool {
	return strings.EqualFold(key.Kind, "Alert") || strings.EqualFold(key.Kind, "Provider") || (strings.EqualFold(key.Kind, "HelmRepository") && strings.EqualFold(stringField(objectMap(object["spec"]), "type"), "oci"))
}

func (a *access) reconcileGraph(ctx context.Context, key objectKey, timestamp string, wait, withSource bool, visited map[objectKey]bool) (map[string]any, error) {
	canonical := objectKey{strings.ToLower(key.Kind), key.Namespace, key.Name}
	if visited[canonical] {
		return nil, unsupported()
	}
	visited[canonical] = true
	target, typ, rep, err := a.target(ctx, key.Kind, key.Namespace)
	if err != nil {
		return nil, err
	}
	record, err := a.get(ctx, target, typ, key.Namespace, key.Name)
	if err != nil {
		return nil, err
	}
	body, err := nativeObject(record)
	if err != nil {
		return nil, err
	}
	if suspended, _ := objectMap(body["spec"])["suspend"].(bool); suspended {
		return nil, unsupported()
	}
	if staticObject(key, body) {
		return nil, unsupported()
	}
	if _, err := a.schema(ctx, typ, rep, "status", "lastHandledReconcileAt"); err != nil {
		return nil, err
	}
	if _, err := a.schema(ctx, typ, rep, "status", "conditions"); err != nil {
		return nil, err
	}
	var source objectKey
	var hasSource bool
	if withSource {
		child, ok, err := sourceOf(key, body)
		if err != nil {
			return nil, err
		}
		source, hasSource = child, ok
		if ok {
			childTarget, childType, _, err := a.target(ctx, child.Kind, child.Namespace)
			if err != nil {
				return nil, err
			}
			childRecord, err := a.get(ctx, childTarget, childType, child.Namespace, child.Name)
			if err != nil {
				return nil, err
			}
			childBody, err := nativeObject(childRecord)
			if err != nil {
				return nil, err
			}
			if suspended, _ := objectMap(childBody["spec"])["suspend"].(bool); suspended {
				return nil, unsupported()
			}
			if !staticObject(child, childBody) {
				if _, err := a.reconcileGraph(ctx, child, timestamp, true, true, visited); err != nil {
					return nil, err
				}
			}
		}
	}
	record, err = a.update(ctx, target, typ, key.Namespace, key.Name, func(body map[string]any) error {
		if suspended, _ := objectMap(body["spec"])["suspend"].(bool); suspended || staticObject(key, body) {
			return unsupported()
		}
		if withSource {
			current, ok, err := sourceOf(key, body)
			if err != nil {
				return err
			}
			if ok != hasSource || current != source {
				return resource.NewError("CONFLICT", "The object changed during the update.")
			}
		}
		metadata := objectMap(body["metadata"])
		if metadata == nil {
			return unsupported()
		}
		annotations := objectMap(metadata["annotations"])
		if annotations == nil {
			annotations = map[string]any{}
			metadata["annotations"] = annotations
		}
		annotations["reconcile.fluxcd.io/requestedAt"] = timestamp
		return nil
	})
	if err != nil {
		return nil, err
	}
	if wait {
		record, err = a.waitReady(ctx, target, typ, key.Namespace, key.Name, timestamp)
		if err != nil {
			return nil, err
		}
	}
	return nativeObject(record)
}

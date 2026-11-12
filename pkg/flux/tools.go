package flux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	resource "github.com/omniviewdev/plugin-sdk/pkg/resource/v2"
	"github.com/omniviewdev/plugin-sdk/pkg/sdk"
	"github.com/omniviewdev/plugin-sdk/pkg/types"
	"github.com/omniviewdev/plugin-sdk/pkg/v1/hostconnections"
	"github.com/omniviewdev/plugin-sdk/pkg/v1/tools"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type input struct {
	Kind           string   `json:"kind"`
	Namespace      string   `json:"namespace"`
	Name           string   `json:"name"`
	Kinds          []string `json:"kinds"`
	WithSource     bool     `json:"with_source"`
	WaitForReady   bool     `json:"wait_for_ready"`
	TimeoutSeconds *int     `json:"timeout_seconds"`
}

func nativeObject(record resource.Record) (map[string]any, error) {
	value, err := resource.NativeFromValue(record.Body)
	if err != nil {
		return nil, err
	}
	object := objectMap(value)
	if object == nil {
		return nil, unsupported()
	}
	return object, nil
}

func Register(plugin *sdk.Plugin) error {
	str := map[string]any{"type": "string", "minLength": 1}
	boolean := map[string]any{"type": "boolean"}
	timeout := map[string]any{"type": "integer", "minimum": 1, "maximum": 90, "default": 60}
	var definitions []tools.Tool
	for _, name := range []string{"status", "reconcile", "suspend", "resume"} {
		props := map[string]any{"namespace": str}
		required := []string{}
		if name == "status" {
			props["kinds"] = map[string]any{"type": "array", "items": str}
		} else {
			props["kind"], props["name"] = str, str
			required = []string{"kind", "namespace", "name"}
		}
		if name == "reconcile" {
			props["with_source"] = boolean
		}
		if name == "reconcile" || name == "resume" {
			props["wait_for_ready"], props["timeout_seconds"] = boolean, timeout
		}
		readOnly, destructive, idempotent := name == "status", false, name == "status" || name == "suspend"
		operation := name
		definitions = append(definitions, tools.Tool{Descriptor: tools.Descriptor{Name: name, Title: strings.Title(name) + " Flux resources", Description: "Inspect or operate on Flux resources.", Scope: tools.ScopeConnection, ConnectionOwner: owner, InputSchema: tools.ObjectSchema(props, required...), Annotations: tools.Annotations{ReadOnly: &readOnly, Destructive: &destructive, Idempotent: &idempotent}}, Handler: func(pc *types.PluginContext, raw json.RawMessage) (*tools.Result, error) {
			return handle(pc, operation, raw)
		}})
	}
	return tools.RegisterPlugin(plugin, tools.PluginOpts{Tools: definitions})
}

func handle(pc *types.PluginContext, operation string, raw json.RawMessage) (*tools.Result, error) {
	result, err := execute(pc, operation, raw)
	return result, mapTimeout(err)
}

func mapTimeout(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return &resource.ResourceOperationError{Code: "TIMEOUT", Message: "The operation timed out.", Err: err}
	}
	return err
}

func execute(pc *types.PluginContext, operation string, raw json.RawMessage) (*tools.Result, error) {
	var in input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return nil, err
	}
	if pc.Connection == nil || pc.Connection.ID == "" {
		return nil, unsupported()
	}
	client, err := hostconnections.FromContext(pc.Context)
	if err != nil {
		return nil, err
	}
	ctx := pc.Context
	if operation != "status" {
		if strings.TrimSpace(in.Kind) == "" || strings.TrimSpace(in.Namespace) == "" || strings.TrimSpace(in.Name) == "" {
			return nil, unsupported()
		}
		seconds := 60
		if in.TimeoutSeconds != nil {
			seconds = *in.TimeoutSeconds
		}
		if seconds < 1 || seconds > 90 {
			return nil, unsupported()
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		defer cancel()
	}
	a, err := openAccess(ctx, client, pc.Connection.ID)
	if err != nil {
		return nil, err
	}
	if operation == "status" {
		result, err := a.status(ctx, in.Namespace, in.Kinds)
		if err != nil {
			return nil, err
		}
		return tools.JSONResult(result)
	}
	key := objectKey{in.Kind, in.Namespace, in.Name}
	if operation == "reconcile" {
		if in.WithSource && !strings.EqualFold(in.Kind, "Kustomization") && !strings.EqualFold(in.Kind, "HelmRelease") && !strings.EqualFold(in.Kind, "HelmChart") {
			return nil, unsupported()
		}
		timestamp := time.Now().UTC().Format(time.RFC3339Nano)
		body, err := a.reconcileGraph(ctx, key, timestamp, in.WaitForReady, in.WithSource, make(map[objectKey]bool))
		if err != nil {
			return nil, err
		}
		state, _ := findingFor(in.Kind, body)
		if ready, _ := readyObservation(body, timestamp); !ready && state.Ready != nil && *state.Ready {
			state.Ready = nil
		}
		return tools.JSONResult(map[string]any{"state": state, "requestedAt": timestamp})
	}
	target, typ, rep, err := a.target(ctx, in.Kind, in.Namespace)
	if err != nil {
		return nil, err
	}
	suspendSchema, err := a.schema(ctx, typ, rep, "spec", "suspend")
	if err != nil {
		return nil, err
	}
	if stringField(suspendSchema, "type") != "boolean" {
		return nil, unsupported()
	}
	if operation == "resume" && in.WaitForReady {
		record, err := a.get(ctx, target, typ, in.Namespace, in.Name)
		if err != nil {
			return nil, err
		}
		body, err := nativeObject(record)
		if err != nil {
			return nil, err
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
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	record, err := a.update(ctx, target, typ, in.Namespace, in.Name, func(body map[string]any) error {
		if operation == "resume" && in.WaitForReady && staticObject(key, body) {
			return unsupported()
		}
		spec := objectMap(body["spec"])
		if spec == nil {
			return unsupported()
		}
		spec["suspend"] = operation == "suspend"
		if operation == "resume" && in.WaitForReady {
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
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if operation == "resume" && in.WaitForReady {
		record, err = a.waitReady(ctx, target, typ, in.Namespace, in.Name, timestamp)
		if err != nil {
			return nil, err
		}
	}
	body, err := nativeObject(record)
	if err != nil {
		return nil, err
	}
	state, _ := findingFor(in.Kind, body)
	if operation == "resume" && in.WaitForReady {
		if ready, _ := readyObservation(body, timestamp); !ready && state.Ready != nil && *state.Ready {
			state.Ready = nil
		}
	}
	if operation == "resume" && in.WaitForReady {
		return tools.JSONResult(map[string]any{"state": state, "requestedAt": timestamp})
	}
	return tools.JSONResult(map[string]any{"state": state})
}

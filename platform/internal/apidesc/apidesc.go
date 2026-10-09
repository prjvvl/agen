// Package apidesc exposes the Agen API descriptors (with source comments) and
// derives MCP tool definitions from them, so every Hub RPC is available over
// REST, Connect and MCP from the same proto source.
package apidesc

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	// Register the generated types so the registry resolves imports.
	_ "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// Built with: buf build spec/proto -o platform/internal/apidesc/agen.binpb
//
//go:embed agen.binpb
var descriptorSet []byte

var (
	loadOnce sync.Once
	files    *protoregistry.Files
	loadErr  error
)

// Files returns the descriptor registry built from the embedded set, which
// keeps source comments (the generated Go code does not).
func Files() (*protoregistry.Files, error) {
	loadOnce.Do(func() {
		var set descriptorpb.FileDescriptorSet
		if loadErr = proto.Unmarshal(descriptorSet, &set); loadErr != nil {
			return
		}
		files, loadErr = protodesc.NewFiles(&set)
	})
	return files, loadErr
}

// Service returns a service descriptor by full name, e.g. "agen.v1.HubService".
func Service(fullName string) (protoreflect.ServiceDescriptor, error) {
	f, err := Files()
	if err != nil {
		return nil, err
	}
	d, err := f.FindDescriptorByName(protoreflect.FullName(fullName))
	if err != nil {
		return nil, err
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a service", fullName)
	}
	return sd, nil
}

// Tool is an MCP tool derived from one unary RPC.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// Procedure is the Connect procedure path, e.g. /agen.v1.HubService/ListNests.
	Procedure string                        `json:"-"`
	Method    protoreflect.MethodDescriptor `json:"-"`
}

// Tools derives one MCP tool per unary method. Tool names are snake_case
// method names (ListDeployments -> list_deployments).
func Tools(sd protoreflect.ServiceDescriptor) []Tool {
	var out []Tool
	methods := sd.Methods()
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		if m.IsStreamingClient() || m.IsStreamingServer() {
			continue
		}
		desc := comment(m)
		if desc == "" {
			desc = string(m.Name())
		}
		out = append(out, Tool{
			Name:        SnakeCase(string(m.Name())),
			Description: desc,
			InputSchema: messageSchema(m.Input(), map[protoreflect.FullName]bool{}),
			Procedure:   fmt.Sprintf("/%s/%s", sd.FullName(), m.Name()),
			Method:      m,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func comment(d protoreflect.Descriptor) string {
	loc := d.ParentFile().SourceLocations().ByDescriptor(d)
	return strings.TrimSpace(strings.Join(strings.Fields(loc.LeadingComments), " "))
}

// SnakeCase converts CamelCase to snake_case.
func SnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// messageSchema builds a JSON Schema matching the protojson encoding.
func messageSchema(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) map[string]any {
	switch md.FullName() {
	case "google.protobuf.Timestamp":
		return map[string]any{"type": "string", "format": "date-time"}
	case "google.protobuf.Struct":
		return map[string]any{"type": "object"}
	case "google.protobuf.Value":
		return map[string]any{}
	}
	if seen[md.FullName()] {
		return map[string]any{"type": "object"}
	}
	seen[md.FullName()] = true
	defer delete(seen, md.FullName())

	props := map[string]any{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		s := fieldSchema(f, seen)
		if c := comment(f); c != "" {
			s["description"] = c
		}
		props[f.JSONName()] = s
	}
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
}

func fieldSchema(f protoreflect.FieldDescriptor, seen map[protoreflect.FullName]bool) map[string]any {
	if f.IsMap() {
		return map[string]any{"type": "object", "additionalProperties": singular(f.MapValue(), seen)}
	}
	if f.IsList() {
		return map[string]any{"type": "array", "items": singular(f, seen)}
	}
	return singular(f, seen)
}

func singular(f protoreflect.FieldDescriptor, seen map[protoreflect.FullName]bool) map[string]any {
	switch f.Kind() {
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return map[string]any{"type": "integer"}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		// protojson encodes 64-bit integers as strings but accepts numbers.
		return map[string]any{"type": []string{"integer", "string"}}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return map[string]any{"type": "number"}
	case protoreflect.StringKind:
		return map[string]any{"type": "string"}
	case protoreflect.BytesKind:
		return map[string]any{"type": "string", "contentEncoding": "base64"}
	case protoreflect.EnumKind:
		vals := f.Enum().Values()
		names := make([]string, 0, vals.Len())
		for i := 0; i < vals.Len(); i++ {
			names = append(names, string(vals.Get(i).Name()))
		}
		return map[string]any{"type": "string", "enum": names}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return messageSchema(f.Message(), seen)
	}
	return map[string]any{}
}

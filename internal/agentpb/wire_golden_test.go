/*
Copyright paul_wtf.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agentpb_test

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/spawnery/spawnery/internal/agentpb"
)

const wireGolden = "testdata/agent.wire.golden"

var update = flag.Bool("update", false, "rewrite "+wireGolden+" from the compiled descriptor")

// The golden pins every field number, name, type and cardinality, every enum
// value and every RPC of agent.proto. An addition fails too, so that each
// change to the wire shows up in a diff someone reviewed.
func TestTheWireFormatMatchesTheGolden(t *testing.T) {
	got := wireLines(agentpb.File_spawnery_agent_v1alpha1_agent_proto)
	if *update {
		if err := os.WriteFile(wireGolden, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", wireGolden, err)
		}
		return
	}
	raw, err := os.ReadFile(wireGolden)
	if err != nil {
		t.Fatalf("read %s: %v", wireGolden, err)
	}
	want := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")

	var gone, added []string
	for _, line := range want {
		if !slices.Contains(got, line) {
			gone = append(gone, line)
		}
	}
	for _, line := range got {
		if !slices.Contains(want, line) {
			added = append(added, line)
		}
	}
	for _, line := range gone {
		t.Errorf("in the golden, not in agent.proto: %s", line)
	}
	for _, line := range added {
		t.Errorf("in agent.proto, not in the golden: %s", line)
	}
	if len(gone) > 0 {
		t.Log("If a number or a type changed or a field vanished, every agent built against the " +
			"golden breaks; reserve a removed number rather than reuse it. A rename alone is wire-compatible.")
	}
	if t.Failed() {
		t.Logf("If the change is meant: go test ./internal/agentpb -run %s -update, and commit the golden.", t.Name())
	}
}

// wireLines lists messages and enums by full name and their entries by
// number, so moving a declaration inside the file changes nothing.
func wireLines(fd protoreflect.FileDescriptor) []string {
	var msgs []protoreflect.MessageDescriptor
	var enums []protoreflect.EnumDescriptor
	var collect func(protoreflect.MessageDescriptors)
	collect = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			m := ms.Get(i)
			if m.IsMapEntry() {
				continue
			}
			msgs = append(msgs, m)
			for j := 0; j < m.Enums().Len(); j++ {
				enums = append(enums, m.Enums().Get(j))
			}
			collect(m.Messages())
		}
	}
	collect(fd.Messages())
	for i := 0; i < fd.Enums().Len(); i++ {
		enums = append(enums, fd.Enums().Get(i))
	}
	slices.SortFunc(msgs, func(a, b protoreflect.MessageDescriptor) int {
		return strings.Compare(string(a.FullName()), string(b.FullName()))
	})
	slices.SortFunc(enums, func(a, b protoreflect.EnumDescriptor) int {
		return strings.Compare(string(a.FullName()), string(b.FullName()))
	})

	var out []string
	for _, m := range msgs {
		var fields []protoreflect.FieldDescriptor
		for j := 0; j < m.Fields().Len(); j++ {
			fields = append(fields, m.Fields().Get(j))
		}
		slices.SortFunc(fields, func(a, b protoreflect.FieldDescriptor) int { return int(a.Number() - b.Number()) })
		for _, f := range fields {
			line := fmt.Sprintf("message %s %d %s %s", m.FullName(), f.Number(), f.Name(), fieldType(f))
			if o := f.ContainingOneof(); o != nil && !o.IsSynthetic() {
				line += " oneof " + string(o.Name())
			}
			out = append(out, line)
		}
		for j := 0; j < m.ReservedRanges().Len(); j++ {
			r := m.ReservedRanges().Get(j)
			// FieldRanges are half-open.
			out = append(out, fmt.Sprintf("message %s reserved %d-%d", m.FullName(), r[0], r[1]-1))
		}
		out = append(out, reservedNames("message", m.FullName(), m.ReservedNames())...)
	}
	for _, e := range enums {
		var values []protoreflect.EnumValueDescriptor
		for j := 0; j < e.Values().Len(); j++ {
			values = append(values, e.Values().Get(j))
		}
		slices.SortStableFunc(values, func(a, b protoreflect.EnumValueDescriptor) int { return int(a.Number() - b.Number()) })
		for _, v := range values {
			out = append(out, fmt.Sprintf("enum %s %d %s", e.FullName(), v.Number(), v.Name()))
		}
		for j := 0; j < e.ReservedRanges().Len(); j++ {
			r := e.ReservedRanges().Get(j)
			out = append(out, fmt.Sprintf("enum %s reserved %d-%d", e.FullName(), r[0], r[1]))
		}
		out = append(out, reservedNames("enum", e.FullName(), e.ReservedNames())...)
	}
	for i := 0; i < fd.Services().Len(); i++ {
		s := fd.Services().Get(i)
		for j := 0; j < s.Methods().Len(); j++ {
			m := s.Methods().Get(j)
			out = append(out, fmt.Sprintf("service %s %s %s%s %s%s", s.FullName(), m.Name(),
				streaming(m.IsStreamingClient()), m.Input().FullName(),
				streaming(m.IsStreamingServer()), m.Output().FullName()))
		}
	}
	return out
}

func fieldType(f protoreflect.FieldDescriptor) string {
	if f.IsMap() {
		return fmt.Sprintf("map<%s,%s>", kindName(f.MapKey()), kindName(f.MapValue()))
	}
	name := kindName(f)
	switch {
	case f.IsList():
		return "repeated " + name
	case f.HasOptionalKeyword():
		return "optional " + name
	}
	return name
}

func kindName(f protoreflect.FieldDescriptor) string {
	switch f.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return string(f.Message().FullName())
	case protoreflect.EnumKind:
		return string(f.Enum().FullName())
	}
	return f.Kind().String()
}

func streaming(on bool) string {
	if on {
		return "stream "
	}
	return ""
}

func reservedNames(kind string, name protoreflect.FullName, names protoreflect.Names) []string {
	var out []string
	for i := 0; i < names.Len(); i++ {
		out = append(out, fmt.Sprintf("%s %s reserved %q", kind, name, names.Get(i)))
	}
	return out
}

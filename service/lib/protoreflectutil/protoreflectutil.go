// Package protoreflectutil holds small protoreflect-based helpers shared
// across more than one service - see AGENTS.md's "a Go helper shared
// across services goes under service/lib" convention.
package protoreflectutil

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// OneofMessage returns the message set in m's oneof field named oneofName,
// as a generic protoreflect.Message, along with that populated field's own
// name (e.g. for api/device/device.proto's Device.details oneof, "sensor",
// "thermostat", ...). ok is false if m has no such oneof, or no field of it
// is currently set.
func OneofMessage(m proto.Message, oneofName string) (msg protoreflect.Message, name protoreflect.Name, ok bool) {
	refl := m.ProtoReflect()
	oneof := refl.Descriptor().Oneofs().ByName(protoreflect.Name(oneofName))
	if oneof == nil {
		return nil, "", false
	}
	fd := refl.WhichOneof(oneof)
	if fd == nil {
		return nil, "", false
	}
	return refl.Get(fd).Message(), fd.Name(), true
}

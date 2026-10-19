package metrics

import (
	"math"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	apiDevice "github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/lib/protoreflectutil"
)

// Leaf is one numeric/bool/enum value found in a device's trait state.
type Leaf struct {
	// Name is the leaf's key within its trait, dotted for nested messages
	// (e.g. "co2_ppm", "colour.hue").
	Name string
	// Continuous is true for int/float leaves, which the sampler applies
	// deadband to. Bools and enums are discrete.
	Continuous bool
	Value      FieldValue
}

// TraitState is the leaves of one trait on a device.
type TraitState struct {
	// Trait is the device message's field name for the trait, e.g.
	// "air_quality" or "on_off" - with no leading device-kind segment, same as
	// the policy GetState key convention.
	Trait  string
	Leaves []Leaf
}

// Flatten walks d's populated trait states, returning each trait's numeric,
// bool and enum leaves. Strings, bytes, lists, maps and well-known types
// (timestamps, durations) are not metrics and are skipped, as is a sensor's
// Metadata (last_reported/low_battery etc. are bookkeeping, not
// measurements).
//
// A device-details field that has a "state" sub-message is a trait and only
// its state is read (attributes describe what's possible, not what's true).
// One without - Sensor.BinarySensor, Sensor.PowerConsumption - has its own
// scalars read directly.
func Flatten(d *apiDevice.Device) []TraitState {
	dm, _, ok := protoreflectutil.OneofMessage(d, "details")
	if !ok {
		return nil
	}

	var out []TraitState
	fields := dm.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.Message() == nil || f.IsList() || f.IsMap() || !dm.Has(f) || f.Name() == "metadata" {
			continue
		}
		tm := dm.Get(f).Message()

		src := tm
		if sf := tm.Descriptor().Fields().ByName("state"); sf != nil && sf.Message() != nil && !sf.IsList() {
			if !tm.Has(sf) {
				continue
			}
			src = tm.Get(sf).Message()
		}

		var leaves []Leaf
		collectLeaves("", src, &leaves)
		if len(leaves) > 0 {
			out = append(out, TraitState{Trait: string(f.Name()), Leaves: leaves})
		}
	}
	return out
}

func collectLeaves(prefix string, m protoreflect.Message, out *[]Leaf) {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.IsList() || f.IsMap() {
			continue
		}
		// An unset optional/message field is absent; a plain proto3 scalar
		// is always present (as its zero value).
		if (f.HasPresence() || f.Message() != nil) && !m.Has(f) {
			continue
		}
		name := prefix + string(f.Name())

		switch f.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if strings.HasPrefix(string(f.Message().FullName()), "google.protobuf.") {
				continue
			}
			collectLeaves(name+".", m.Get(f).Message(), out)
		case protoreflect.BoolKind:
			*out = append(*out, Leaf{Name: name, Value: FieldValue{Kind: KindBool, Bool: m.Get(f).Bool()}})
		case protoreflect.EnumKind:
			*out = append(*out, Leaf{Name: name, Value: FieldValue{Kind: KindInt, Int: int64(m.Get(f).Enum())}})
		case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
			protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
			*out = append(*out, Leaf{Name: name, Continuous: true, Value: FieldValue{Kind: KindInt, Int: m.Get(f).Int()}})
		case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			*out = append(*out, Leaf{Name: name, Continuous: true, Value: FieldValue{Kind: KindInt, Int: int64(m.Get(f).Uint())}})
		case protoreflect.FloatKind, protoreflect.DoubleKind:
			// A NaN/Inf reading is "no measurement"; it would also poison the
			// sampler's last-written value, since NaN compares false to everything.
			if v := m.Get(f).Float(); !math.IsNaN(v) && !math.IsInf(v, 0) {
				*out = append(*out, Leaf{Name: name, Continuous: true, Value: FieldValue{Kind: KindFloat, Float: v}})
			}
		}
	}
}

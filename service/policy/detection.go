package policy

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
	"go.uber.org/zap"
)

// Detection is one object a camera-like device is currently tracking, as
// reported by its Detection trait.
type Detection struct {
	Label string
	// Score is the classifier's confidence, 0..1. It does not reliably separate real objects from
	// false ones, so prefer other gates (RequireStillImage, Zone) over a high MinScore.
	Score         float64
	EventID       string
	FirstSeen     time.Time
	HasStillImage bool
	Zones         []string
	SubLabel      string
	SubLabelScore float64
}

// StillImage is one picture fetched from a device, see HomeAPI.GetStillImage.
type StillImage struct {
	Data        []byte
	ContentType string
	// Source is "event_snapshot" or "latest_frame".
	Source  string
	EventID string
}

// DetectionVisibleParams parameterizes the "detection.visible" condition type: true while the device is
// tracking at least one object that matches every non-zero field. It is the gate for "something real is
// there and a picture of it exists" - deliberately with no notion of who it is (a later, separate
// condition would handle named people).
//
// Reacts to the per-device "device.updated.<id>" signal rather than polling.
type DetectionVisibleParams struct {
	DeviceID string
	// Label, if set, restricts to objects with this label (e.g. "person").
	Label string
	// MinScore, if > 0, requires Score >= MinScore.
	MinScore float64
	// RequireStillImage requires that a picture of the object can already be fetched.
	RequireStillImage bool
	// Zone, if set, requires the object to be in this zone.
	Zone string
}

func (p DetectionVisibleParams) matches(d Detection) bool {
	if p.Label != "" && d.Label != p.Label {
		return false
	}
	if p.MinScore > 0 && d.Score < p.MinScore {
		return false
	}
	if p.RequireStillImage && !d.HasStillImage {
		return false
	}
	if p.Zone != "" && !slices.Contains(d.Zones, p.Zone) {
		return false
	}
	return true
}

func registerDetectionConditionTypes(e *Engine) {
	RegisterConditionType(e.registry, "detection.visible", func(p DetectionVisibleParams) Condition {
		var mu sync.Mutex
		var last bool

		fn := func() bool {
			detections, err := e.home.GetDetections(p.DeviceID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				e.logger.Warn("detection.visible: GetDetections failed, holding last value",
					zap.String("deviceId", p.DeviceID), zap.Error(err))
				return last
			}

			last = false
			for _, d := range detections {
				if p.matches(d) {
					last = true
					break
				}
			}
			return last
		}
		return NewPredicateCondition(e.bus, "device.updated."+p.DeviceID, fn)
	})
}

// detectionToLua converts d to the table home.getDetections returns.
func detectionToLua(L *lua.LState, d Detection) *lua.LTable {
	t := L.NewTable()
	t.RawSetString("label", lua.LString(d.Label))
	t.RawSetString("score", lua.LNumber(d.Score))
	t.RawSetString("event_id", lua.LString(d.EventID))
	t.RawSetString("has_still_image", lua.LBool(d.HasStillImage))
	t.RawSetString("sub_label", lua.LString(d.SubLabel))
	t.RawSetString("sub_label_score", lua.LNumber(d.SubLabelScore))
	if !d.FirstSeen.IsZero() {
		t.RawSetString("first_seen", lua.LNumber(d.FirstSeen.Unix()))
	}
	zones := L.NewTable()
	for _, z := range d.Zones {
		zones.Append(lua.LString(z))
	}
	t.RawSetString("zones", zones)
	return t
}

// stillImageMaxHeight reads the optional max_height from a home.stillImage options table.
func stillImageMaxHeight(L *lua.LState, opts *lua.LTable) (int, error) {
	if opts == nil {
		return 0, nil
	}
	switch v := L.GetField(opts, "max_height").(type) {
	case lua.LNumber:
		if v < 0 || v != lua.LNumber(int(v)) {
			return 0, fmt.Errorf("max_height must be a non-negative integer")
		}
		return int(v), nil
	case *lua.LNilType:
		return 0, nil
	default:
		return 0, fmt.Errorf("max_height must be a number")
	}
}

func stillImageToLua(L *lua.LState, img *StillImage) *lua.LTable {
	t := L.NewTable()
	t.RawSetString("data", lua.LString(img.Data))
	t.RawSetString("content_type", lua.LString(img.ContentType))
	t.RawSetString("source", lua.LString(img.Source))
	t.RawSetString("event_id", lua.LString(img.EventID))
	return t
}

// cidSafe lowercases and trims a script-supplied content id.
func cidSafe(s string) string { return strings.TrimSpace(s) }

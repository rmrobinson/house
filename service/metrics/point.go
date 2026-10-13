// Package metrics implements metricsd's core: turning BridgeService updates
// into InfluxDB points. It is the only package in this repo that knows about
// Influx - house itself stays storage-free.
package metrics

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FieldKind selects which FieldValue member is set.
type FieldKind int

const (
	KindFloat FieldKind = iota
	KindInt
	KindBool
	KindString
)

// FieldValue is one Influx field value: exactly one of the typed members is
// meaningful, selected by Kind.
type FieldValue struct {
	Kind  FieldKind
	Float float64
	Int   int64
	Bool  bool
	Str   string
}

func (v FieldValue) asFloat() float64 {
	switch v.Kind {
	case KindInt:
		return float64(v.Int)
	case KindBool:
		if v.Bool {
			return 1
		}
		return 0
	}
	return v.Float
}

// Point is one line of Influx line protocol.
type Point struct {
	Measurement string
	Tags        map[string]string
	Fields      map[string]FieldValue
	Time        time.Time
}

// Influx line protocol escapes differ by position (measurement, tag
// key/value and field key, string field value).
//
// Newlines are replaced with spaces everywhere: one terminates a line, and a
// single malformed line makes Influx reject its whole batch.
var (
	measurementEscaper = strings.NewReplacer(",", `\,`, " ", `\ `, "\n", `\ `, "\r", `\ `)
	keyEscaper         = strings.NewReplacer(",", `\,`, "=", `\=`, " ", `\ `, "\n", `\ `, "\r", `\ `)
	stringEscaper      = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ", "\r", " ")
)

// Line encodes p as line protocol (nanosecond precision), or "" if p has no
// fields, which line protocol cannot represent. Tags and fields are emitted in
// sorted key order so the output is deterministic. Tags with empty values are
// omitted, since line protocol rejects them.
func (p Point) Line() string {
	// Non-finite floats have no line-protocol encoding and Influx rejects the
	// whole batch containing one, so they are left out.
	fieldKeys := make([]string, 0, len(p.Fields))
	for k, v := range p.Fields {
		if v.Kind == KindFloat && (math.IsNaN(v.Float) || math.IsInf(v.Float, 0)) {
			continue
		}
		fieldKeys = append(fieldKeys, k)
	}
	if len(fieldKeys) == 0 {
		return ""
	}
	sort.Strings(fieldKeys)

	var b strings.Builder
	b.WriteString(measurementEscaper.Replace(p.Measurement))

	tagKeys := make([]string, 0, len(p.Tags))
	for k, v := range p.Tags {
		if v != "" {
			tagKeys = append(tagKeys, k)
		}
	}
	sort.Strings(tagKeys)
	for _, k := range tagKeys {
		b.WriteByte(',')
		b.WriteString(keyEscaper.Replace(k))
		b.WriteByte('=')
		b.WriteString(keyEscaper.Replace(p.Tags[k]))
	}

	b.WriteByte(' ')
	for i, k := range fieldKeys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(keyEscaper.Replace(k))
		b.WriteByte('=')
		v := p.Fields[k]
		switch v.Kind {
		case KindFloat:
			b.WriteString(strconv.FormatFloat(v.Float, 'g', -1, 64))
		case KindInt:
			b.WriteString(strconv.FormatInt(v.Int, 10))
			b.WriteByte('i')
		case KindBool:
			b.WriteString(strconv.FormatBool(v.Bool))
		case KindString:
			b.WriteByte('"')
			b.WriteString(stringEscaper.Replace(v.Str))
			b.WriteByte('"')
		}
	}

	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(p.Time.UnixNano(), 10))
	return b.String()
}

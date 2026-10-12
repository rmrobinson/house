package metrics

import (
	"math"
	"sync"
	"time"
)

// SamplingMode selects whether the sampler filters at all.
type SamplingMode string

const (
	// ModeDeadband writes a field when it moves by at least its deadband,
	// or when its heartbeat elapses.
	ModeDeadband SamplingMode = "deadband"
	// ModeNone writes every live value as received.
	ModeNone SamplingMode = "none"
)

// SamplingConfig configures Sampler. Use DefaultSampling as the starting
// point; the zero value has no heartbeat.
type SamplingConfig struct {
	Mode SamplingMode
	// Heartbeat forces a write once a field has gone this long unwritten,
	// so a gap in the series means "no data", not "no change". Zero disables.
	Heartbeat time.Duration
	// Deadband maps a leaf name ("co2_ppm") or "trait.leaf"
	// ("temperature.value", which wins) to the minimum absolute change that
	// is written.
	Deadband map[string]float64
	// DefaultDeadband applies to continuous leaves with no Deadband entry.
	// Zero means any change is written.
	DefaultDeadband float64
}

// DefaultSampling returns the deadband/heartbeat defaults from the metrics
// plan, keyed by the actual proto leaf names.
func DefaultSampling() SamplingConfig {
	return SamplingConfig{
		Mode:      ModeDeadband,
		Heartbeat: 5 * time.Minute,
		Deadband: map[string]float64{
			"temperature_c":                  0.1,
			"temperature.value":              0.1,
			"humidity_percentage":            1,
			"pressure_hpa":                   1,
			"co2_ppm":                        10,
			"volatile_organic_compounds_ppb": 10,
			"radon_bq_m3":                    5,
			"radon_short_term_bq_m3":         5,
			"power_w":                        1,
			"capacity_remaining_pct":         1,
		},
	}
}

type sampleKey struct {
	device, trait, leaf string
}

type lastWritten struct {
	value float64
	at    time.Time
}

// Sampler decides which live leaf values are worth writing. Backfilled
// samples bypass it entirely - they are historical facts, not a stream to
// thin out. Safe for concurrent use.
type Sampler struct {
	cfg SamplingConfig

	mu   sync.Mutex
	last map[sampleKey]lastWritten
}

// NewSampler creates a Sampler.
func NewSampler(cfg SamplingConfig) *Sampler {
	if cfg.Mode == "" {
		cfg.Mode = ModeDeadband
	}
	return &Sampler{cfg: cfg, last: make(map[sampleKey]lastWritten)}
}

// Filter returns the subset of leaves that should be written for deviceID's
// trait at now, and records them as written.
//
// A leaf is written when it is first seen since process start, when a
// continuous leaf has moved by at least its deadband from the last written
// value, when a discrete leaf has changed at all, or when the heartbeat has
// elapsed. Discrete leaves are change-filtered rather than always written
// (the plan's wording) because a device update re-carries every leaf: a
// power-metering light updating once a second would otherwise rewrite its
// on/off state once a second too.
func (s *Sampler) Filter(deviceID, trait string, leaves []Leaf, now time.Time) []Leaf {
	if s.cfg.Mode == ModeNone {
		return leaves
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Leaf
	for _, l := range leaves {
		k := sampleKey{deviceID, trait, l.Name}
		v := l.Value.asFloat()
		prev, seen := s.last[k]

		write := !seen
		if seen {
			switch {
			case s.cfg.Heartbeat > 0 && now.Sub(prev.at) >= s.cfg.Heartbeat:
				write = true
			case l.Continuous:
				write = v != prev.value && math.Abs(v-prev.value) >= s.deadband(trait, l.Name)
			default:
				write = v != prev.value
			}
		}
		if write {
			s.last[k] = lastWritten{value: v, at: now}
			out = append(out, l)
		}
	}
	return out
}

// Forget drops everything remembered about deviceID, e.g. after it is
// removed, so the map doesn't grow without bound.
func (s *Sampler) Forget(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.last {
		if k.device == deviceID {
			delete(s.last, k)
		}
	}
}

func (s *Sampler) deadband(trait, leaf string) float64 {
	if d, ok := s.cfg.Deadband[trait+"."+leaf]; ok {
		return d
	}
	if d, ok := s.cfg.Deadband[leaf]; ok {
		return d
	}
	return s.cfg.DefaultDeadband
}

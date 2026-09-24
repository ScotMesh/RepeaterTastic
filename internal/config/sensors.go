package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

// Sensors offers host readings to hosted identities as sensors of their own. Everything here can be
// written in the YAML file or changed in the GUI, which saves it back to the same file: there is one
// source of truth (see docs/sensors.md).
type Sensors struct {
	// Interval is how often a hosted node broadcasts environment telemetry. 0 = 1h, minimum 30m,
	// which is the firmware's own floor and keeps a mast full of identities off the air.
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	// Sources are the readings this host has: a command to run, a file to read, or a push from a
	// plugin or the API.
	Sources []sensors.Source `yaml:"sources,omitempty" json:"sources"`
	// Attach offers a source to identities, each publishing it as its own sensor.
	Attach []SensorAttach `yaml:"attach,omitempty" json:"attach"`
}

// SensorAttach offers one source to one or more identities.
type SensorAttach struct {
	Sensor string `yaml:"sensor" json:"sensor"`
	// Identities names who publishes it: short names, node ids like !a1c40e07, or "all".
	Identities []string `yaml:"identities" json:"identities"`
	// Fields limits what this identity publishes; empty means everything the source reports.
	Fields []sensors.Field `yaml:"fields,omitempty" json:"fields,omitempty"`
}

// SensorInterval is the environment telemetry interval to ask hosted nodes for.
func (s Sensors) SensorInterval() time.Duration {
	if s.Interval == 0 {
		return time.Hour
	}
	return s.Interval
}

// Enabled reports whether any source is configured at all.
func (s Sensors) Enabled() bool { return len(s.Sources) > 0 }

// Source finds a source by id.
func (s Sensors) Source(id string) (sensors.Source, bool) {
	for _, src := range s.Sources {
		if src.ID == id {
			return src, true
		}
	}
	return sensors.Source{}, false
}

// For lists what one identity publishes. An identity is named by its short name (case-insensitive)
// or its node id; "all" matches every identity. Fields are narrowed to what a chip can carry, so a
// caller can hand the result straight to PlanChips.
func (s Sensors) For(shortName, nodeID string) []sensors.Attachment {
	var out []sensors.Attachment
	for _, a := range s.Attach {
		if !a.matches(shortName, nodeID) {
			continue
		}
		out = append(out, sensors.Attachment{Sensor: a.Sensor, Fields: carried(a.Fields)})
	}
	return out
}

// matches reports whether this attachment names the identity.
func (a SensorAttach) matches(shortName, nodeID string) bool {
	for _, want := range a.Identities {
		want = strings.TrimSpace(want)
		if strings.EqualFold(want, "all") ||
			(shortName != "" && strings.EqualFold(want, shortName)) ||
			(nodeID != "" && strings.EqualFold(want, nodeID)) {
			return true
		}
	}
	return false
}

// carried drops fields no imitated chip can carry; validation has already warned about them.
func carried(fields []sensors.Field) []sensors.Field {
	var out []sensors.Field
	for _, f := range fields {
		if sensors.Carried(f) {
			out = append(out, f)
		}
	}
	return out
}

// validateSensors checks the sensors section and fills its defaults.
func (c *Config) validateSensors() error {
	if iv := c.Sensors.Interval; iv != 0 && iv < 30*time.Minute {
		return errors.New("sensors.interval must be 0 (1h) or at least 30m: the firmware ignores anything shorter")
	}
	seen := map[string]bool{}
	for i := range c.Sensors.Sources {
		s := &c.Sensors.Sources[i]
		if err := s.Validate(); err != nil {
			return fmt.Errorf("sensors.sources: %w", err)
		}
		if seen[s.ID] {
			return fmt.Errorf("sensors.sources: two sensors are called %q", s.ID)
		}
		seen[s.ID] = true
	}
	for _, a := range c.Sensors.Attach {
		if err := c.validateAttach(a, seen); err != nil {
			return err
		}
	}
	return nil
}

// validateAttach checks one attachment names a real sensor, some identity, and fields we can carry.
func (c *Config) validateAttach(a SensorAttach, known map[string]bool) error {
	if !known[a.Sensor] {
		return fmt.Errorf("sensors.attach: no sensor called %q is configured", a.Sensor)
	}
	if len(a.Identities) == 0 {
		return fmt.Errorf("sensors.attach %q: name the identities that publish it, or \"all\"", a.Sensor)
	}
	for _, f := range a.Fields {
		if !f.Known() {
			return fmt.Errorf("sensors.attach %q: unknown field %q", a.Sensor, f)
		}
		if !sensors.Carried(f) {
			return fmt.Errorf("sensors.attach %q: no imitated chip can carry %q (see docs/sensors.md)", a.Sensor, f)
		}
	}
	return nil
}

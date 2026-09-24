// Package sensors reads sensor values on the host and offers them to hosted nodes as sensors of
// their own.
//
// RepeaterTastic never sends a telemetry packet. It samples a source once, writes the current
// readings into each node's directory, and a small shim inside that node's meshtasticd answers the
// node's I²C reads from that file (see shim/). The node detects the "sensor" at start-up, reads it
// on its own schedule, and broadcasts and answers requests entirely by itself — so one real sensor
// can appear to as many identities as you like, each publishing it as its own.
package sensors

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Field is a reading we can carry, named as Meshtastic names it.
type Field string

const (
	Temperature Field = "temperature"  // °C
	Humidity    Field = "humidity"     // % relative
	Pressure    Field = "pressure"     // hPa
	Lux         Field = "lux"          // lx
	Voltage     Field = "voltage"      // V
	Current     Field = "current"      // A
	PM10        Field = "pm10"         // µg/m³
	PM25        Field = "pm25"         // µg/m³
	PM100       Field = "pm100"        // µg/m³
	Distance    Field = "distance"     // mm
	Radiation   Field = "radiation"    // µR/h
	Rainfall1h  Field = "rainfall_1h"  // mm
	Rainfall24h Field = "rainfall_24h" // mm
)

// Fields lists every field in a stable order, for the GUI and for validation.
var Fields = []Field{Temperature, Humidity, Pressure, Lux, Voltage, Current,
	PM10, PM25, PM100, Distance, Radiation, Rainfall1h, Rainfall24h}

// Unit is the unit a field is quoted in, for display.
func (f Field) Unit() string {
	switch f {
	case Temperature:
		return "°C"
	case Humidity:
		return "%"
	case Pressure:
		return "hPa"
	case Lux:
		return "lx"
	case Voltage:
		return "V"
	case Current:
		return "A"
	case PM10, PM25, PM100:
		return "µg/m³"
	case Distance:
		return "mm"
	case Radiation:
		return "µR/h"
	case Rainfall1h, Rainfall24h:
		return "mm"
	}
	return ""
}

// Known reports whether f is a field we can carry.
func (f Field) Known() bool {
	for _, k := range Fields {
		if k == f {
			return true
		}
	}
	return false
}

// Reading is one sample from a source.
type Reading struct {
	At     time.Time         `json:"at"`
	Fields map[Field]float64 `json:"fields"`
}

// Kind is how a source produces readings.
type Kind string

const (
	// Push: another program POSTs readings to the API. Nothing is scheduled here.
	Push Kind = "push"
	// Exec: run a command on an interval; it prints readings on stdout.
	Exec Kind = "exec"
	// File: read a file on an interval; something else keeps it up to date.
	File Kind = "file"
)

// Source is one sensor as configured, in the YAML file or from the GUI.
type Source struct {
	ID       string        `yaml:"id" json:"id"`
	Name     string        `yaml:"name,omitempty" json:"name"`
	Kind     Kind          `yaml:"kind" json:"kind"`
	Command  string        `yaml:"command,omitempty" json:"command,omitempty"`   // Exec
	Path     string        `yaml:"path,omitempty" json:"path,omitempty"`         // File
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"` // Exec, File; default 1m
	// Scale multiplies a field as it arrives, for sources that report in another unit.
	Scale map[Field]float64 `yaml:"scale,omitempty" json:"scale,omitempty"`
}

// Validate checks a source is usable and fills defaults.
func (s *Source) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("a sensor needs an id")
	}
	if strings.ContainsAny(s.ID, " /\\\t") {
		return fmt.Errorf("sensor id %q: use letters, digits and dashes", s.ID)
	}
	if s.Name == "" {
		s.Name = s.ID
	}
	switch s.Kind {
	case Push:
	case Exec:
		if s.Command == "" {
			return fmt.Errorf("sensor %q: kind exec needs a command to run", s.ID)
		}
	case File:
		if s.Path == "" {
			return fmt.Errorf("sensor %q: kind file needs a path to read", s.ID)
		}
	default:
		return fmt.Errorf("sensor %q: kind must be push, exec or file, not %q", s.ID, s.Kind)
	}
	if s.Kind != Push {
		if s.Interval == 0 {
			s.Interval = time.Minute
		}
		if s.Interval < 5*time.Second {
			return fmt.Errorf("sensor %q: read it no more often than every 5s", s.ID)
		}
	}
	for f := range s.Scale {
		if !f.Known() {
			return fmt.Errorf("sensor %q: unknown field %q", s.ID, f)
		}
	}
	return nil
}

// Attachment offers a source to one identity, as that node's own sensor. Fields empty means every
// field the source reports that a chip can carry.
type Attachment struct {
	Sensor string  `yaml:"sensor" json:"sensor"`
	Fields []Field `yaml:"fields,omitempty" json:"fields,omitempty"`
}

// Chip is an I²C sensor the shim can imitate. Which chip carries a field is fixed: the firmware
// merges every sensor into one packet and the last writer wins, so we imitate exactly one chip per
// field (see docs/sensors.md).
type Chip struct {
	Name   string  // as the shim's I2CSHIM_CHIPS knows it
	Addr   uint8   // its I²C address
	Fields []Field // what it can carry
}

// Chips are the imitations the shim implements, in the order they are planned.
var Chips = []Chip{
	{"pct2075", 0x37, []Field{Temperature}},
	{"aht10", 0x38, []Field{Humidity}},
	{"bh1750", 0x23, []Field{Lux}},
	{"pmsa003i", 0x12, []Field{PM10, PM25, PM100}},
	{"ina226", 0x40, []Field{Voltage, Current}},
	{"rcwl9620", 0x57, []Field{Distance}},
	{"cgradsens", 0x66, []Field{Radiation}},
	{"dfrobot_rain", 0x1D, []Field{Rainfall1h, Rainfall24h}},
}

// PlanChips picks the chips a node needs to carry fields, and reports any field no chip can carry.
func PlanChips(fields []Field) (chips []Chip, unsupported []Field) {
	want := map[Field]bool{}
	for _, f := range fields {
		want[f] = true
	}
	for _, c := range Chips {
		for _, f := range c.Fields {
			if want[f] {
				chips = append(chips, c)
				break
			}
		}
	}
	for _, f := range fields {
		if !Carried(f) {
			unsupported = append(unsupported, f)
		}
	}
	return chips, unsupported
}

// Carried reports whether any chip can carry f.
func Carried(f Field) bool {
	for _, c := range Chips {
		for _, cf := range c.Fields {
			if cf == f {
				return true
			}
		}
	}
	return false
}

// ChipNames is the I2CSHIM_CHIPS value for a plan.
func ChipNames(chips []Chip) string {
	names := make([]string, len(chips))
	for i, c := range chips {
		names[i] = c.Name
	}
	return strings.Join(names, ",")
}

// RenderValues writes the readings file the shim reads: one "field=value" per line, sorted so an
// unchanged reading writes an unchanged file.
func RenderValues(r Reading, only []Field) []byte {
	keep := map[Field]bool{}
	for _, f := range only {
		keep[f] = true
	}
	lines := make([]string, 0, len(r.Fields))
	for f, v := range r.Fields {
		if len(only) > 0 && !keep[f] {
			continue
		}
		if !Carried(f) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s=%.4f", f, v))
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// ParseValues reads "field=value", "field: value" or JSON-ish lines, as the shim does, so a source
// and the shim agree on the format. Unknown fields are ignored.
func ParseValues(b []byte) map[Field]float64 {
	out := map[Field]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "{},"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var key, val string
		if i := strings.IndexAny(line, "=:"); i > 0 {
			key, val = line[:i], line[i+1:]
		} else {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		val = strings.Trim(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val), ",")), `"'`)
		f := Field(strings.ToLower(key))
		if !f.Known() {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(val, "%g", &v); err != nil {
			continue
		}
		out[f] = v
	}
	return out
}

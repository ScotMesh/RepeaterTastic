// The sensors a hosted node carries. RepeaterTastic never sends a telemetry packet: it writes the
// readings into the node's own directory and a shim inside that meshtasticd answers the node's I²C
// reads from the file, so the node detects the "sensor", reads it and broadcasts it itself
// (docs/sensors.md, internal/sensors, shim/).

package nodes

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

// SensorSource is where a hosted node's readings come from: the host's sensor registry. It is an
// interface so this package doesn't reach into the configuration. A nil source means the feature is
// off and nothing here happens.
type SensorSource interface {
	// For is what the identity with these names publishes (node ID as "!aabbccdd").
	For(shortName, nodeID string) []sensors.Attachment
	// Latest is the newest reading of a source, or false when it hasn't been read yet.
	Latest(id string) (sensors.Reading, bool)
	// Subscribe reports the ID of each source as a new reading lands, until the stop is called.
	Subscribe(buf int) (<-chan string, func())
	// Interval is how often each node should broadcast environment telemetry.
	Interval() time.Duration
}

// MinEnvInterval is the shortest environment telemetry interval a node is asked for: the firmware's
// own floor is half an hour, and every identity publishing a sensor pays for it in airtime
// (docs/sensors.md).
const MinEnvInterval = 30 * time.Minute

// The files an instance directory holds for its sensors. The directory is the node's own: Docker
// mounts it at /data, a meshtasticd binary sees it where it is.
const (
	valuesFile = "sensors.values" // field=value lines, rewritten as readings arrive
	shimFile   = "i2cshim.so"     // the LD_PRELOAD library, extracted from the binary
)

// devicePath is the I²C bus a node carrying sensors is told to use. It must not exist in the kernel
// (shim/README.md): the shim answers the node's open before it gets there. Every node names the same
// bus, because each one only ever fakes it inside its own process.
const devicePath = "/dev/i2c-fake0"

// SensorSetup is the imitated I²C sensors one instance carries. It is planned before the process
// starts, because meshtasticd scans the bus once at start-up and then throws the scanner away.
type SensorSetup struct {
	// Attach is what the identity publishes, with each attachment's fields resolved.
	Attach []sensors.Attachment
	// Fields is the union of the attachments' fields, sorted: what the values file holds.
	Fields []sensors.Field
	// Chips are the chips the shim presents for those fields.
	Chips []sensors.Chip
	// Device is the fake I²C bus the node opens, in its config file and its environment.
	Device string
}

// Sources are the sensors the node publishes, in the order they were attached.
func (s *SensorSetup) Sources() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Attach))
	for _, a := range s.Attach {
		out = append(out, a.Sensor)
	}
	return out
}

// AirQuality reports whether the node carries particulate readings. They reach the air through the
// firmware's air quality module, which is a separate telemetry setting, and one that has to be on
// before the node scans the bus (shim/README.md).
func (s *SensorSetup) AirQuality() bool {
	if s == nil {
		return false
	}
	for _, f := range s.Fields {
		switch f {
		case sensors.PM10, sensors.PM25, sensors.PM100:
			return true
		}
	}
	return false
}

// same reports whether two plans would give a node the same sensors. Only a difference here is
// worth restarting a node for.
func (s *SensorSetup) same(o *SensorSetup) bool {
	if s == nil || o == nil {
		return s == nil && o == nil
	}
	if len(s.Fields) != len(o.Fields) || len(s.Attach) != len(o.Attach) {
		return false
	}
	for i, f := range s.Fields {
		if o.Fields[i] != f {
			return false
		}
	}
	for i, a := range s.Attach {
		b := o.Attach[i]
		if a.Sensor != b.Sensor || len(a.Fields) != len(b.Fields) {
			return false
		}
		for j, f := range a.Fields {
			if b.Fields[j] != f {
				return false
			}
		}
	}
	return true
}

// publishes reports whether the node carries readings from source id.
func (s *SensorSetup) publishes(id string) bool {
	if s == nil {
		return false
	}
	for _, a := range s.Attach {
		if a.Sensor == id {
			return true
		}
	}
	return false
}

// planSensors is the setup for an identity's attachments: the fields it publishes and the chips
// that carry them. It returns nil when the identity publishes nothing, which is the common case.
//
// An attachment with no fields of its own publishes everything its source reports; a source that
// has never been read yet reports nothing, so such an attachment carries nothing until the node is
// restarted (the GUI restarts it when a sensor is attached).
func planSensors(src SensorSource, atts []sensors.Attachment, logf func(string, ...any)) *SensorSetup {
	if src == nil || len(atts) == 0 {
		return nil
	}
	if logf == nil {
		logf = discardLogf
	}
	p := &SensorSetup{}
	seen := map[sensors.Field]bool{}
	for _, a := range atts {
		fields := a.Fields
		if len(fields) == 0 {
			if rd, ok := src.Latest(a.Sensor); ok {
				for f := range rd.Fields {
					fields = append(fields, f)
				}
			}
		}
		keep := make([]sensors.Field, 0, len(fields))
		for _, f := range fields {
			if !sensors.Carried(f) {
				logf("sensors: %s: no chip can carry %s; it isn't published", a.Sensor, f)
				continue
			}
			keep = append(keep, f)
			if !seen[f] {
				seen[f] = true
				p.Fields = append(p.Fields, f)
			}
		}
		if len(keep) == 0 {
			continue
		}
		sortFields(keep)
		p.Attach = append(p.Attach, sensors.Attachment{Sensor: a.Sensor, Fields: keep})
	}
	if len(p.Fields) == 0 {
		return nil
	}
	sortFields(p.Fields)
	p.Chips, _ = sensors.PlanChips(p.Fields)
	return p
}

// sortFields puts fields in a stable order, so a plan and the file it renders don't depend on map
// iteration.
func sortFields(f []sensors.Field) {
	sort.Slice(f, func(i, j int) bool { return f[i] < f[j] })
}

// valuesFor renders the readings file for a plan: every field the node publishes, from the latest
// reading of the source that carries it. A field with no reading yet is written as 0, because the
// file has to answer the node's start-up scan before the first sample has arrived.
func valuesFor(src SensorSource, p *SensorSetup) []byte {
	rd := sensors.Reading{At: time.Now(), Fields: map[sensors.Field]float64{}}
	for _, f := range p.Fields {
		rd.Fields[f] = 0
	}
	for _, a := range p.Attach {
		latest, ok := src.Latest(a.Sensor)
		if !ok {
			continue
		}
		for _, f := range a.Fields {
			if v, ok := latest.Fields[f]; ok {
				rd.Fields[f] = v
			}
		}
	}
	return sensors.RenderValues(rd, p.Fields)
}

// valuesPath is a node's readings file, on the host (we write it; the node reads its own copy of
// the same directory).
func valuesPath(dir string) string { return filepath.Join(dir, valuesFile) }

// launcherRoot is the instance directory as the node's own process sees it: Docker mounts it at
// /data (DockerLauncher.Run), anything else runs on the host and sees the real path.
func launcherRoot(l Launcher, in Instance) string {
	if _, ok := l.(DockerLauncher); ok {
		return "/data"
	}
	// Absolute, because LD_PRELOAD is read by the loader, not by the shell we start the node from.
	if abs, err := filepath.Abs(in.Dir); err == nil {
		return abs
	}
	return in.Dir
}

// seedSensors puts everything the shim needs in the instance directory and fills in the instance's
// environment, before meshtasticd starts: the library, the readings file and the fake bus it opens.
// It runs again before a restart, so a sensor attached to a running identity is in place by the time
// the node next scans for it.
func seedSensors(l Launcher, in *Instance, src SensorSource, p *SensorSetup) error {
	if p == nil {
		in.Sensors, in.Env = nil, nil
		// Leave nothing for a restarted node to read: without this it would keep answering from the
		// last file it was given, publishing a reading that stopped being true when the sensor was
		// taken away.
		if err := os.Remove(valuesPath(in.Dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("sensors for %s: %w", in.Name, err)
		}
		return nil
	}
	if err := os.MkdirAll(in.Dir, 0o700); err != nil {
		return fmt.Errorf("sensors for %s: %w", in.Name, err)
	}
	if err := extractShim(in.Dir); err != nil {
		return err
	}
	if _, err := sensors.WriteValuesFile(valuesPath(in.Dir), valuesFor(src, p)); err != nil {
		return err
	}
	root := launcherRoot(l, *in)
	p.Device = devicePath
	in.Sensors = p
	in.Env = []string{
		"LD_PRELOAD=" + filepath.Join(root, shimFile),
		"I2CSHIM_DEV=" + p.Device,
		"I2CSHIM_VALUES=" + filepath.Join(root, valuesFile),
		"I2CSHIM_CHIPS=" + sensors.ChipNames(p.Chips),
		// Line-buffered, so what the node prints reaches the log tail in the GUI promptly.
		"I2CSHIM_LINEBUF=1",
	}
	return nil
}

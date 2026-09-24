package config

import (
	"testing"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

const sensorYAML = `
sensors:
    interval: 1h
    sources:
        - id: shed
          name: Shed
          kind: exec
          command: /usr/local/bin/read-shed
          interval: 5m
        - id: rack
          kind: push
    attach:
        - sensor: shed
          identities: [BASE, "!a1c40e07"]
          fields: [temperature, humidity]
        - sensor: rack
          identities: [all]
`

func TestSensorsLoadAndSelect(t *testing.T) {
	c, err := load(t, sensorYAML)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Sensors.Enabled() || len(c.Sensors.Sources) != 2 {
		t.Fatalf("sources = %+v", c.Sensors.Sources)
	}
	if src, ok := c.Sensors.Source("shed"); !ok || src.Interval != 5*time.Minute || src.Kind != sensors.Exec {
		t.Fatalf("shed source = %+v (%v)", src, ok)
	}
	// A push source has no interval of its own; the sensors interval is what nodes are told.
	if got := c.Sensors.SensorInterval(); got != time.Hour {
		t.Fatalf("interval %v", got)
	}

	cases := []struct {
		short, node string
		want        []string // sensor ids, in order
	}{
		{"BASE", "!deadbeef", []string{"shed", "rack"}},
		{"base", "", []string{"shed", "rack"}},           // short names are case-insensitive
		{"OTHER", "!a1c40e07", []string{"shed", "rack"}}, // matched by node id
		{"OTHER", "!00000001", []string{"rack"}},         // only the "all" attachment
	}
	for _, tc := range cases {
		got := c.Sensors.For(tc.short, tc.node)
		if len(got) != len(tc.want) {
			t.Fatalf("%s/%s: %d attachments, want %d (%+v)", tc.short, tc.node, len(got), len(tc.want), got)
		}
		for i, want := range tc.want {
			if got[i].Sensor != want {
				t.Errorf("%s/%s: attachment %d = %q, want %q", tc.short, tc.node, i, got[i].Sensor, want)
			}
		}
	}
	if fields := c.Sensors.For("BASE", "")[0].Fields; len(fields) != 2 || fields[0] != sensors.Temperature {
		t.Fatalf("shed fields = %v", fields)
	}
}

func TestSensorsDefaultInterval(t *testing.T) {
	c, err := load(t, "sensors:\n    sources:\n        - {id: a, kind: push}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sensors.SensorInterval(); got != time.Hour {
		t.Fatalf("default interval %v, want 1h", got)
	}
}

func TestSensorsValidation(t *testing.T) {
	expectLoadErrors(t, map[string]string{
		"sensors:\n    interval: 5m\n":                                                                                                   "at least 30m",
		"sensors:\n    sources:\n        - {id: a, kind: exec}\n":                                                                        "needs a command",
		"sensors:\n    sources:\n        - {id: a, kind: file}\n":                                                                        "needs a path",
		"sensors:\n    sources:\n        - {id: a, kind: wishful}\n":                                                                     "push, exec or file",
		"sensors:\n    sources:\n        - {kind: push}\n":                                                                               "needs an id",
		"sensors:\n    sources:\n        - {id: a b, kind: push}\n":                                                                      "letters, digits, dashes",
		"sensors:\n    sources:\n        - {id: a, kind: file, path: /p, interval: 1s}\n":                                                "no more often than every 5s",
		"sensors:\n    sources:\n        - {id: a, kind: push}\n        - {id: a, kind: push}\n":                                         "two sensors are called",
		"sensors:\n    attach:\n        - {sensor: ghost, identities: [all]}\n":                                                          "no sensor called",
		"sensors:\n    sources:\n        - {id: a, kind: push}\n    attach:\n        - {sensor: a, identities: []}\n":                    "name the identities",
		"sensors:\n    sources:\n        - {id: a, kind: push}\n    attach:\n        - {sensor: a, identities: [all], fields: [mood]}\n": "unknown field",
	})
	expectLoadOK(t, "sensors:\n    sources:\n        - {id: a, kind: push}\n    attach:\n        - {sensor: a, identities: [all]}\n")
}

// A sensors section must survive a save and reload unchanged: the GUI writes the same file.
func TestSensorsRoundTrip(t *testing.T) {
	c, err := load(t, sensorYAML)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Sensors.Sources) != 2 || len(back.Sensors.Attach) != 2 ||
		back.Sensors.Sources[0].Command != "/usr/local/bin/read-shed" ||
		back.Sensors.Attach[0].Fields[1] != sensors.Humidity ||
		back.Sensors.SensorInterval() != time.Hour {
		t.Fatalf("round trip lost something: %+v", back.Sensors)
	}
}

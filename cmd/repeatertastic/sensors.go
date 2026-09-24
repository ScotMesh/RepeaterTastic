package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/nodes"
	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

// sensorHub joins the two halves of the sensors feature: the registry, which reads the host's
// sensors, and the configuration, which says who publishes what. Hosted nodes ask it both questions
// (nodes.SensorSource), so it keeps its own copy of the sensors section: the web server owns the
// config it edits, and hands the new one over here when it saves.
type sensorHub struct {
	reg *sensors.Registry

	mu  sync.RWMutex
	set config.Sensors
}

var _ nodes.SensorSource = (*sensorHub)(nil)

// newSensorHub starts the registry with the configured sources.
func newSensorHub(cs config.Sensors, log *slog.Logger) (*sensorHub, error) {
	h := &sensorHub{reg: sensors.New(log)}
	if err := h.Set(cs); err != nil {
		return nil, err
	}
	return h, nil
}

// Run samples the sources until ctx ends.
func (h *sensorHub) Run(ctx context.Context) { h.reg.Run(ctx) }

// Registry is the registry the web server serves. A nil hub means sensors are off, which is how
// tests start a radio without them.
func (h *sensorHub) Registry() *sensors.Registry {
	if h == nil {
		return nil
	}
	return h.reg
}

// Set takes a new sensors section: the sources go to the registry, the attachments are what hosted
// nodes are told from now on.
func (h *sensorHub) Set(cs config.Sensors) error {
	if h == nil {
		return nil
	}
	if err := h.reg.Apply(cs.Sources); err != nil {
		return err
	}
	h.mu.Lock()
	h.set = cs
	h.mu.Unlock()
	return nil
}

// For lists what one identity publishes.
func (h *sensorHub) For(shortName, nodeID string) []sensors.Attachment {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.set.For(shortName, nodeID)
}

// Latest is a source's current reading.
func (h *sensorHub) Latest(id string) (sensors.Reading, bool) { return h.reg.Latest(id) }

// Subscribe reports source ids as readings land.
func (h *sensorHub) Subscribe(buf int) (<-chan string, func()) { return h.reg.Subscribe(buf) }

// Interval is how often hosted nodes should broadcast environment telemetry.
func (h *sensorHub) Interval() time.Duration {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.set.SensorInterval()
}

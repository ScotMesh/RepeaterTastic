package plugins

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

// SensorHost is the part of the sensor registry plugins may use: see what the host reads, and give a
// reading to a sensor the operator made for pushing (docs/sensors.md).
type SensorHost interface {
	Status() []sensors.Status
	Push(id string, fields map[sensors.Field]float64) error
}

// ListSensors answers with the host's sensors and their latest readings.
func (h *hostServer) ListSensors(ctx context.Context, _ *pluginv1.ListSensorsRequest) (*pluginv1.ListSensorsResponse, error) {
	if _, err := h.granted(ctx, permSensorsPublish); err != nil {
		return nil, err
	}
	reg := h.m.opt.Sensors
	if reg == nil {
		return nil, status.Error(codes.Unavailable, "this host has no sensors")
	}
	out := &pluginv1.ListSensorsResponse{}
	for _, st := range reg.Status() {
		out.Sensors = append(out.Sensors, sensorProto(st))
	}
	return out, nil
}

// PublishSensor gives a reading to one of the host's push sensors. The identities it is attached to
// broadcast it themselves; nothing is transmitted here, so this costs no airtime budget.
func (h *hostServer) PublishSensor(ctx context.Context, req *pluginv1.PublishSensorRequest) (*pluginv1.PublishSensorResponse, error) {
	if _, err := h.granted(ctx, permSensorsPublish); err != nil {
		return nil, err
	}
	reg := h.m.opt.Sensors
	if reg == nil {
		return nil, status.Error(codes.Unavailable, "this host has no sensors")
	}
	fields := map[sensors.Field]float64{}
	for k, v := range req.GetFields() {
		fields[sensors.Field(k)] = v
	}
	if len(fields) == 0 {
		return nil, status.Error(codes.InvalidArgument, "give at least one field, such as temperature")
	}
	if err := reg.Push(req.GetSensorId(), fields); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pluginv1.PublishSensorResponse{}, nil
}

// sensorProto describes one sensor to a plugin.
func sensorProto(st sensors.Status) *pluginv1.Sensor {
	s := &pluginv1.Sensor{Id: st.ID, Name: st.Name, Kind: string(st.Kind), Fields: map[string]float64{}}
	for f, v := range st.Last.Fields {
		s.Fields[string(f)] = v
	}
	if !st.Last.At.IsZero() {
		s.ReadAtUnix = st.Last.At.Unix()
	}
	return s
}

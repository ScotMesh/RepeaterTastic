package plugins

import (
	"testing"

	"google.golang.org/grpc/codes"

	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

// withSensors gives the manager a registry with one push and one exec sensor.
func withSensors(t *testing.T) (func(*Options), *sensors.Registry) {
	t.Helper()
	reg := sensors.New(quietLog())
	if err := reg.Apply([]sensors.Source{
		{ID: "weather", Name: "Weather", Kind: sensors.Push},
		{ID: "shed", Kind: sensors.Exec, Command: "true"},
	}); err != nil {
		t.Fatal(err)
	}
	return func(o *Options) { o.Sensors = reg }, reg
}

func TestPluginPublishesSensor(t *testing.T) {
	with, reg := withSensors(t)
	e := newAPIEnv(t, with)

	// Without the permission, neither call works.
	noPerm := e.attach(t, "nosy", "nodes.read")
	e.open(t, noPerm, hello("nosy", ""))
	_, err := e.client.PublishSensor(noPerm, &pluginv1.PublishSensorRequest{SensorId: "weather", Fields: map[string]float64{"temperature": 3}})
	wantCode(t, "publish without sensors.publish", err, codes.PermissionDenied)
	_, err = e.client.ListSensors(noPerm, &pluginv1.ListSensorsRequest{})
	wantCode(t, "list without sensors.publish", err, codes.PermissionDenied)

	ctx := e.attach(t, "weatherman", "sensors.publish")
	e.open(t, ctx, hello("weatherman", ""))
	push := func(id string, fields map[string]float64) error {
		_, err := e.client.PublishSensor(ctx, &pluginv1.PublishSensorRequest{SensorId: id, Fields: fields})
		return err
	}
	wantCode(t, "unknown sensor", push("ghost", map[string]float64{"temperature": 3}), codes.InvalidArgument)
	wantCode(t, "not a push sensor", push("shed", map[string]float64{"temperature": 3}), codes.InvalidArgument)
	wantCode(t, "no fields", push("weather", nil), codes.InvalidArgument)

	if err := push("weather", map[string]float64{"temperature": 18.5, "humidity": 61, "mood": 7}); err != nil {
		t.Fatal(err)
	}
	rd, ok := reg.Latest("weather")
	if !ok || rd.Fields[sensors.Temperature] != 18.5 || rd.Fields[sensors.Humidity] != 61 {
		t.Fatalf("reading %+v (%v)", rd, ok)
	}
	if _, unknown := rd.Fields["mood"]; unknown {
		t.Error("an unknown field was kept")
	}

	list, err := e.client.ListSensors(ctx, &pluginv1.ListSensorsRequest{})
	if err != nil || len(list.Sensors) != 2 {
		t.Fatalf("list %v %v", list, err)
	}
	var weather *pluginv1.Sensor
	for _, s := range list.Sensors {
		if s.Id == "weather" {
			weather = s
		}
	}
	if weather == nil || weather.Name != "Weather" || weather.Kind != "push" ||
		weather.Fields["temperature"] != 18.5 || weather.ReadAtUnix == 0 {
		t.Fatalf("weather %+v", weather)
	}
}

// A host with no sensors says so rather than pretending the call worked.
func TestPublishSensorWithoutRegistry(t *testing.T) {
	e := newAPIEnv(t)
	ctx := e.attach(t, "weatherman", "sensors.publish")
	e.open(t, ctx, hello("weatherman", ""))
	_, err := e.client.PublishSensor(ctx, &pluginv1.PublishSensorRequest{SensorId: "weather", Fields: map[string]float64{"temperature": 1}})
	wantCode(t, "no registry", err, codes.Unavailable)
	_, err = e.client.ListSensors(ctx, &pluginv1.ListSensorsRequest{})
	wantCode(t, "no registry", err, codes.Unavailable)
}

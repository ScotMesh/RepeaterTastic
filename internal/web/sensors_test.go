package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

// sensorEnv is a web server with a sensor registry and a config file on disk.
type sensorEnv struct {
	*testEnv
	reg *sensors.Registry
	tok string
}

func newSensorEnv(t *testing.T) *sensorEnv {
	t.Helper()
	reg := sensors.New(slog.New(slog.DiscardHandler))
	env := newTestEnv(t, func(o *Options) {
		o.Sensors = reg
		// The daemon hands saved sections to its sensor hub; here the registry is the whole of it.
		o.SensorsChanged = func(cs config.Sensors) error { return reg.Apply(cs.Sources) }
	})
	return &sensorEnv{testEnv: env, reg: reg, tok: env.signIn(t)}
}

// reload reads the config file back from disk, as the daemon would at its next start: the test of
// whether a control really saved.
func (e *sensorEnv) reload(t *testing.T) config.Sensors {
	t.Helper()
	cfg, err := config.Load(e.cfg.Path())
	if err != nil {
		t.Fatalf("the saved config file no longer loads: %v", err)
	}
	return cfg.Sensors
}

// addIdentity creates an identity and returns its node id.
func (e *sensorEnv) addIdentity(t *testing.T, long, short string) string {
	t.Helper()
	code, obj, _ := call(t, e.srv, "POST", "/api/v1/identities", e.tok, map[string]any{"long_name": long, "short_name": short})
	if code != http.StatusCreated {
		t.Fatalf("create %s: %d %v", short, code, obj)
	}
	return obj["node_id"].(string)
}

// writeFileSensor is a file the registry can read, so tests don't depend on a shell.
func writeFileSensor(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "reading")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSensorsCRUDRoundTrip adds, reads, renames and deletes a sensor through the API, and checks
// the config file on disk says the same thing every time.
func TestSensorsCRUDRoundTrip(t *testing.T) {
	env := newSensorEnv(t)
	path := writeFileSensor(t, "temperature=18.4\nhumidity=63.2\n")

	code, obj, _ := call(t, env.srv, "GET", "/api/v1/sensors", env.tok, nil)
	if code != http.StatusOK || obj["enabled"] != true || obj["interval"] != "1h0m0s" {
		t.Fatalf("empty list: %d %v", code, obj)
	}
	if len(obj["sensors"].([]any)) != 0 {
		t.Fatalf("sensors before adding any: %v", obj["sensors"])
	}
	// The catalogue tells the GUI which fields no chip can carry (pressure, today).
	chips := map[string]string{}
	for _, f := range obj["fields"].([]any) {
		m := f.(map[string]any)
		chips[m["field"].(string)] = m["chip"].(string)
	}
	if chips["temperature"] != "pct2075" || chips["pressure"] != "bmp280" || chips["radiation"] != "" {
		t.Fatalf("field catalogue: %v", chips)
	}

	body := map[string]any{"id": "shed", "name": "Shed", "kind": "file", "path": path, "interval": "30s"}
	code, made, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok, body)
	if code != http.StatusCreated || made["name"] != "Shed" || made["interval"] != "30s" || made["last"] != nil {
		t.Fatalf("add: %d %v", code, made)
	}
	if ids := made["identities"].([]any); len(ids) != 0 {
		t.Fatalf("a new sensor is published by nobody, got %v", ids)
	}
	if got := env.reload(t); len(got.Sources) != 1 || got.Sources[0].ID != "shed" || got.Sources[0].Path != path {
		t.Fatalf("saved sources: %+v", got.Sources)
	}
	if len(env.reg.Sources()) != 1 {
		t.Fatalf("the registry did not take the new sensor: %+v", env.reg.Sources())
	}

	// The same id twice is a conflict, not a silent overwrite.
	if code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok, body); code != http.StatusConflict {
		t.Fatalf("duplicate id: %d %v", code, obj)
	}

	// Read now shows exactly what came back.
	code, read, _ := call(t, env.srv, "POST", "/api/v1/sensors/shed/read", env.tok, nil)
	if code != http.StatusOK {
		t.Fatalf("read now: %d %v", code, read)
	}
	last := read["last"].(map[string]any)
	if f := last["fields"].(map[string]any); f["temperature"] != 18.4 || f["humidity"] != 63.2 {
		t.Fatalf("reading: %v", last)
	}
	if read["fresh"] != true || read["reads"].(float64) != 1 {
		t.Fatalf("after a read: %v", read)
	}

	// History has it, oldest first.
	code, _, hist := call(t, env.srv, "GET", "/api/v1/sensors/shed/history?since=1h", env.tok, nil)
	if code != http.StatusOK || len(hist) != 1 {
		t.Fatalf("history: %d %v", code, hist)
	}

	// An edit that renames follows into the attachments, or the config file would stop loading.
	id := env.addIdentity(t, "Base Camp", "BASE")
	if code, _, _ := call(t, env.srv, "PUT", "/api/v1/identities/"+id+"/sensors", env.tok,
		[]map[string]any{{"sensor": "shed", "fields": []string{"temperature"}}}); code != http.StatusOK {
		t.Fatalf("attach: %d", code)
	}
	code, edited, _ := call(t, env.srv, "PUT", "/api/v1/sensors/shed", env.tok,
		map[string]any{"id": "shed-roof", "name": "Shed roof", "kind": "file", "path": path, "interval": "1m"})
	if code != http.StatusOK || edited["id"] != "shed-roof" || edited["interval"] != "1m0s" {
		t.Fatalf("edit: %d %v", code, edited)
	}
	got := env.reload(t)
	if len(got.Sources) != 1 || got.Sources[0].ID != "shed-roof" || got.Sources[0].Name != "Shed roof" {
		t.Fatalf("saved after rename: %+v", got.Sources)
	}
	if len(got.Attach) != 1 || got.Attach[0].Sensor != "shed-roof" {
		t.Fatalf("a rename must follow into the attachments: %+v", got.Attach)
	}

	// Deleting detaches it from every identity.
	code, del, _ := call(t, env.srv, "DELETE", "/api/v1/sensors/shed-roof", env.tok, nil)
	if code != http.StatusOK || del["ok"] != true {
		t.Fatalf("delete: %d %v", code, del)
	}
	if got := env.reload(t); len(got.Sources) != 0 || len(got.Attach) != 0 {
		t.Fatalf("after delete: %+v %+v", got.Sources, got.Attach)
	}
	if code, _, _ := call(t, env.srv, "DELETE", "/api/v1/sensors/shed-roof", env.tok, nil); code != http.StatusNotFound {
		t.Fatalf("deleting twice: %d", code)
	}
}

// TestSensorsRefusals checks every refusal says something the operator can act on.
func TestSensorsRefusals(t *testing.T) {
	env := newSensorEnv(t)
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no id", map[string]any{"kind": "push"}, "needs an id"},
		{"unknown kind", map[string]any{"id": "x", "kind": "magic"}, "push, exec or file"},
		{"exec with no command", map[string]any{"id": "x", "kind": "exec"}, "needs a command"},
		{"file with no path", map[string]any{"id": "x", "kind": "file"}, "needs a path"},
		{"too often", map[string]any{"id": "x", "kind": "exec", "command": "true", "interval": "1s"}, "no more often than every 5s"},
		{"interval nonsense", map[string]any{"id": "x", "kind": "exec", "command": "true", "interval": "soon"}, "length of time"},
		{"unknown field", map[string]any{"id": "x", "kind": "push", "scale": map[string]float64{"loudness": 2}}, "no reading called loudness"},
		{"reserved id", map[string]any{"id": "interval", "kind": "push"}, "another id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok, tc.body)
			if code != http.StatusBadRequest || !contains(obj["error"], tc.want) {
				t.Fatalf("got %d %v, want 400 mentioning %q", code, obj, tc.want)
			}
		})
	}
	if code, obj, _ := call(t, env.srv, "PUT", "/api/v1/sensors/nope", env.tok,
		map[string]any{"id": "nope", "kind": "push"}); code != http.StatusNotFound {
		t.Fatalf("editing a sensor that isn't there: %d %v", code, obj)
	}

	// The telemetry interval has a floor the firmware itself keeps.
	if code, obj, _ := call(t, env.srv, "PUT", "/api/v1/sensors/interval", env.tok, map[string]any{"interval": "10m"}); code != http.StatusBadRequest ||
		!contains(obj["error"], "every 30m") {
		t.Fatalf("interval under 30m: %d %v", code, obj)
	}
	code, obj, _ := call(t, env.srv, "PUT", "/api/v1/sensors/interval", env.tok, map[string]any{"interval": "45m"})
	if code != http.StatusOK || obj["interval"] != "45m0s" {
		t.Fatalf("interval: %d %v", code, obj)
	}
	if got := env.reload(t); got.SensorInterval().String() != "45m0s" {
		t.Fatalf("the interval did not save: %v", got.Interval)
	}
}

// TestSensorPush pushes readings to a push sensor and refuses the rest.
func TestSensorPush(t *testing.T) {
	env := newSensorEnv(t)
	if code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok,
		map[string]any{"id": "rain", "kind": "push"}); code != http.StatusCreated {
		t.Fatalf("add push sensor: %d %v", code, obj)
	}
	if code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok,
		map[string]any{"id": "cpu", "kind": "file", "path": writeFileSensor(t, "temperature=40\n")}); code != http.StatusCreated {
		t.Fatalf("add file sensor: %d %v", code, obj)
	}

	// Unknown fields are ignored; a body with nothing usable is refused.
	code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors/rain/push", env.tok,
		map[string]any{"rainfall_1h": 2.5, "loudness": 11, "humidity": "wet"})
	if code != http.StatusOK {
		t.Fatalf("push: %d %v", code, obj)
	}
	fields := obj["last"].(map[string]any)["fields"].(map[string]any)
	if len(fields) != 1 || fields["rainfall_1h"] != 2.5 {
		t.Fatalf("pushed fields: %v", fields)
	}
	if code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors/rain/push", env.tok, map[string]any{"loudness": 11}); code != http.StatusBadRequest ||
		!contains(obj["error"], "no readings we can carry") {
		t.Fatalf("push with nothing usable: %d %v", code, obj)
	}
	if code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors/cpu/push", env.tok, map[string]any{"temperature": 1}); code != http.StatusBadRequest ||
		!contains(obj["error"], "only a push sensor") {
		t.Fatalf("push to a file sensor: %d %v", code, obj)
	}
	if code, _, _ := call(t, env.srv, "POST", "/api/v1/sensors/ghost/push", env.tok, map[string]any{"temperature": 1}); code != http.StatusNotFound {
		t.Fatalf("push to a sensor that isn't there: %d", code)
	}
	// A push sensor has nothing of its own to read.
	if code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors/cpu/read", env.tok, nil); code != http.StatusOK {
		t.Fatalf("read now on a file sensor: %d %v", code, obj)
	}
}

// TestSensorReadNowReportsWhatCameBack checks a wrong command is obvious straight away.
func TestSensorReadNowReportsWhatCameBack(t *testing.T) {
	env := newSensorEnv(t)
	if code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok,
		map[string]any{"id": "oops", "kind": "file", "path": filepath.Join(t.TempDir(), "missing")}); code != http.StatusCreated {
		t.Fatalf("add: %d %v", code, obj)
	}
	code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors/oops/read", env.tok, nil)
	if code != http.StatusBadRequest || !contains(obj["error"], "nothing at") {
		t.Fatalf("read a file that isn't there: %d %v", code, obj)
	}
	// The failure is remembered against the sensor, for the list.
	_, list, _ := call(t, env.srv, "GET", "/api/v1/sensors", env.tok, nil)
	got := list["sensors"].([]any)[0].(map[string]any)
	if got["errors"].(float64) != 1 || !contains(got["error"], "nothing at") {
		t.Fatalf("remembered error: %v", got)
	}
}

// TestIdentitySensorsAttach attaches a sensor to identities one at a time and checks each one keeps
// what it had.
func TestIdentitySensorsAttach(t *testing.T) {
	env := newSensorEnv(t)
	path := writeFileSensor(t, "temperature=18.4\nhumidity=63.2\n")
	for _, id := range []string{"shed", "loft"} {
		if code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok,
			map[string]any{"id": id, "kind": "file", "path": path}); code != http.StatusCreated {
			t.Fatalf("add %s: %d %v", id, code, obj)
		}
	}
	base := env.addIdentity(t, "Base Camp", "BASE")
	ops := env.addIdentity(t, "Ops Desk", "OPS")

	// Nothing to start with.
	if code, _, arr := call(t, env.srv, "GET", "/api/v1/identities/"+base+"/sensors", env.tok, nil); code != http.StatusOK || len(arr) != 0 {
		t.Fatalf("before attaching: %d %v", code, arr)
	}

	code, _, arr := call(t, env.srv, "PUT", "/api/v1/identities/"+base+"/sensors", env.tok,
		[]map[string]any{{"sensor": "shed", "fields": []string{"temperature"}}})
	if code != http.StatusOK || len(arr) != 1 {
		t.Fatalf("attach to base: %d %v", code, arr)
	}
	if code, _, arr := call(t, env.srv, "PUT", "/api/v1/identities/"+ops+"/sensors", env.tok,
		[]map[string]any{{"sensor": "shed", "fields": []string{"temperature"}}, {"sensor": "loft"}}); code != http.StatusOK || len(arr) != 2 {
		t.Fatalf("attach to ops: %d %v", code, arr)
	}

	// Both publish shed on the same fields, so they share one line; loft is ops's alone.
	got := env.reload(t)
	if len(got.Attach) != 2 {
		t.Fatalf("attachments: %+v", got.Attach)
	}
	shed := attachFor(t, got, "shed")
	if len(shed.Identities) != 2 || !hasString(shed.Identities, base) || !hasString(shed.Identities, ops) {
		t.Fatalf("shed identities: %+v", shed)
	}
	if loft := attachFor(t, got, "loft"); len(loft.Identities) != 1 || loft.Identities[0] != ops {
		t.Fatalf("loft identities: %+v", loft)
	}

	// Base drops shed: ops keeps it.
	if code, _, arr := call(t, env.srv, "PUT", "/api/v1/identities/"+base+"/sensors", env.tok, []map[string]any{}); code != http.StatusOK || len(arr) != 0 {
		t.Fatalf("detach base: %d %v", code, arr)
	}
	got = env.reload(t)
	if shed := attachFor(t, got, "shed"); len(shed.Identities) != 1 || shed.Identities[0] != ops {
		t.Fatalf("ops should still publish shed: %+v", got.Attach)
	}
	if code, _, arr := call(t, env.srv, "GET", "/api/v1/identities/"+ops+"/sensors", env.tok, nil); code != http.StatusOK || len(arr) != 2 {
		t.Fatalf("ops's sensors: %d %v", code, arr)
	}
	// And the list says who publishes what.
	_, list, _ := call(t, env.srv, "GET", "/api/v1/sensors", env.tok, nil)
	for _, raw := range list["sensors"].([]any) {
		sn := raw.(map[string]any)
		if ids := sn["identities"].([]any); len(ids) != 1 || ids[0] != ops {
			t.Fatalf("%v is published by %v, want just ops", sn["id"], ids)
		}
	}
}

// TestIdentitySensorsSplitsAll edits one identity's sensors when the config file said "all", and
// checks the others keep theirs.
func TestIdentitySensorsSplitsAll(t *testing.T) {
	env := newSensorEnv(t)
	path := writeFileSensor(t, "temperature=18.4\n")
	if code, _, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok,
		map[string]any{"id": "shed", "kind": "file", "path": path}); code != http.StatusCreated {
		t.Fatal("add shed")
	}
	base := env.addIdentity(t, "Base Camp", "BASE")
	ops := env.addIdentity(t, "Ops Desk", "OPS")

	// As a hand-written config file would have it: everyone publishes it.
	env.s.cfgMu.Lock()
	env.s.cfg.Sensors.Attach = []config.SensorAttach{{Sensor: "shed", Identities: []string{"all"}}}
	env.s.cfgMu.Unlock()

	if code, _, arr := call(t, env.srv, "PUT", "/api/v1/identities/"+base+"/sensors", env.tok, []map[string]any{}); code != http.StatusOK || len(arr) != 0 {
		t.Fatalf("base leaves: %d %v", code, arr)
	}
	got := env.reload(t)
	shed := attachFor(t, got, "shed")
	if hasString(shed.Identities, base) {
		t.Fatalf("base still publishes shed: %+v", shed)
	}
	if !hasString(shed.Identities, ops) {
		t.Fatalf("ops lost shed when base left: %+v", shed)
	}
	// The relay persona was in "all" too, and keeps it.
	if len(shed.Identities) != 2 {
		t.Fatalf(`"all" should have been written out as the other identities: %+v`, shed)
	}
	if code, _, arr := call(t, env.srv, "GET", "/api/v1/identities/"+ops+"/sensors", env.tok, nil); code != http.StatusOK || len(arr) != 1 {
		t.Fatalf("ops's sensors: %d %v", code, arr)
	}
}

// TestIdentitySensorsRefusals: a sensor that isn't there, a field no chip can carry, one sensor
// twice.
func TestIdentitySensorsRefusals(t *testing.T) {
	env := newSensorEnv(t)
	if code, _, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok,
		map[string]any{"id": "shed", "kind": "push"}); code != http.StatusCreated {
		t.Fatal("add shed")
	}
	id := env.addIdentity(t, "Base Camp", "BASE")
	cases := []struct {
		name string
		body []map[string]any
		want string
	}{
		{"no such sensor", []map[string]any{{"sensor": "ghost"}}, "no sensor called ghost"},
		{"no chip carries it", []map[string]any{{"sensor": "shed", "fields": []string{"radiation"}}}, "no node can publish radiation"},
		{"unknown field", []map[string]any{{"sensor": "shed", "fields": []string{"loudness"}}}, "no reading called loudness"},
		{"twice", []map[string]any{{"sensor": "shed"}, {"sensor": "shed"}}, "listed twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, obj, _ := call(t, env.srv, "PUT", "/api/v1/identities/"+id+"/sensors", env.tok, tc.body)
			if code != http.StatusBadRequest || !contains(obj["error"], tc.want) {
				t.Fatalf("got %d %v, want 400 mentioning %q", code, obj, tc.want)
			}
		})
	}
	if code, _, _ := call(t, env.srv, "PUT", "/api/v1/identities/!deadbeef/sensors", env.tok, []map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("an identity that isn't here: %d", code)
	}
}

// TestSetIdentityAttachments covers the splitting on its own: every other identity must keep what
// it publishes, however the entry was written.
func TestSetIdentityAttachments(t *testing.T) {
	const me, other = "!a1c40e07", "!00ff1234"
	temp := []sensors.Field{sensors.Temperature}
	cases := []struct {
		name   string
		before []config.SensorAttach
		want   []sensors.Attachment
		after  []config.SensorAttach
	}{
		{
			name:   "adds a line",
			before: nil,
			want:   []sensors.Attachment{{Sensor: "shed", Fields: temp}},
			after:  []config.SensorAttach{{Sensor: "shed", Identities: []string{me}, Fields: temp}},
		},
		{
			name:   "splits a shared line",
			before: []config.SensorAttach{{Sensor: "shed", Identities: []string{me, other}}},
			want:   nil,
			after:  []config.SensorAttach{{Sensor: "shed", Identities: []string{other}}},
		},
		{
			name:   "drops a line that was only mine",
			before: []config.SensorAttach{{Sensor: "shed", Identities: []string{"BASE"}}},
			want:   nil,
			after:  []config.SensorAttach{},
		},
		{
			name:   `writes out "all"`,
			before: []config.SensorAttach{{Sensor: "shed", Identities: []string{"all"}}},
			want:   nil,
			after:  []config.SensorAttach{{Sensor: "shed", Identities: []string{other}}},
		},
		{
			// An identity keeping what "all" gives it must not turn "all" into a list, or a sensor
			// offered to every identity would stop reaching the ones added later.
			name:   `keeps "all" when nothing about this identity changes`,
			before: []config.SensorAttach{{Sensor: "shed", Identities: []string{"all"}, Fields: temp}},
			want:   []sensors.Attachment{{Sensor: "shed", Fields: temp}},
			after:  []config.SensorAttach{{Sensor: "shed", Identities: []string{"all"}, Fields: temp}},
		},
		{
			name:   `writes out "all" when this identity wants other fields`,
			before: []config.SensorAttach{{Sensor: "shed", Identities: []string{"all"}}},
			want:   []sensors.Attachment{{Sensor: "shed", Fields: temp}},
			after: []config.SensorAttach{{Sensor: "shed", Identities: []string{other}},
				{Sensor: "shed", Identities: []string{me}, Fields: temp}},
		},
		{
			name:   "joins a line that already says this",
			before: []config.SensorAttach{{Sensor: "shed", Identities: []string{other}, Fields: temp}},
			want:   []sensors.Attachment{{Sensor: "shed", Fields: temp}},
			after:  []config.SensorAttach{{Sensor: "shed", Identities: []string{other, me}, Fields: temp}},
		},
		{
			name:   "keeps a line with other fields apart",
			before: []config.SensorAttach{{Sensor: "shed", Identities: []string{other}}},
			want:   []sensors.Attachment{{Sensor: "shed", Fields: temp}},
			after: []config.SensorAttach{{Sensor: "shed", Identities: []string{other}},
				{Sensor: "shed", Identities: []string{me}, Fields: temp}},
		},
		{
			name:   "leaves other sensors alone",
			before: []config.SensorAttach{{Sensor: "loft", Identities: []string{other}}, {Sensor: "shed", Identities: []string{me}}},
			want:   nil,
			after:  []config.SensorAttach{{Sensor: "loft", Identities: []string{other}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := setIdentityAttachments(tc.before, "BASE", me, []string{other}, tc.want)
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(tc.after)
			if string(a) != string(b) {
				t.Fatalf("got %s, want %s", a, b)
			}
		})
	}
}

// TestSensorsOffAnswers503 checks a host with no sensors says so once and refuses the rest.
func TestSensorsOffAnswers503(t *testing.T) {
	env := newTestEnv(t, nil)
	tok := env.signIn(t)
	code, obj, _ := call(t, env.srv, "GET", "/api/v1/sensors", tok, nil)
	if code != http.StatusOK || obj["enabled"] != false || len(obj["sensors"].([]any)) != 0 {
		t.Fatalf("list with sensors off: %d %v", code, obj)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/sensors"},
		{"PUT", "/api/v1/sensors/x"},
		{"DELETE", "/api/v1/sensors/x"},
		{"POST", "/api/v1/sensors/x/read"},
		{"POST", "/api/v1/sensors/x/push"},
		{"GET", "/api/v1/sensors/x/history"},
		{"PUT", "/api/v1/sensors/interval"},
	} {
		if code, _, _ := call(t, env.srv, c.method, c.path, tok, map[string]any{}); code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s with sensors off: %d", c.method, c.path, code)
		}
	}
	// And nothing answers without a token.
	if code, _, _ := call(t, env.srv, "GET", "/api/v1/sensors", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", code)
	}
}

// TestSensorEvents checks a change reaches every radio's stream once, as a sensor event.
func TestSensorEvents(t *testing.T) {
	env := newSensorEnv(t)
	if code, _, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok,
		map[string]any{"id": "shed", "name": "Shed", "kind": "push"}); code != http.StatusCreated {
		t.Fatal("add shed")
	}
	payload, ok := env.s.eventPayload(radioEvent{env.s.radios[0], mesh.Event{Type: "sensor", Data: "shed"}}, env.s.radios)
	if !ok {
		t.Fatal("a sensor event was dropped")
	}
	if m := payload.(map[string]any); m["name"] != "Shed" || m["kind"] != "push" {
		t.Fatalf("sensor event: %v", payload)
	}
	gone, ok := env.s.sensorEventPayload(mesh.Event{Type: "sensor", Data: "ghost"})
	if !ok || gone.(map[string]any)["deleted"] != true {
		t.Fatalf("a deleted sensor should be announced as gone: %v %v", gone, ok)
	}

	// A reading on the registry becomes an event on every radio's bus.
	sub, unsub := env.s.radios[0].host.Bus.Subscribe(8)
	defer unsub()
	reads, unwatch := env.reg.Subscribe(4)
	defer unwatch()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.s.fanOutSensors(ctx, reads)
	if err := env.reg.Push("shed", map[sensors.Field]float64{sensors.Temperature: 3}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-sub:
			if e.Type == "sensor" && e.Data == "shed" {
				return
			}
		case <-deadline:
			t.Fatal("no sensor event reached the bus")
		}
	}
}

// TestSensorSaveFailureIsReported: a config file that can't be written is a 500 the GUI can show,
// not a silent loss.
func TestSensorSaveFailureIsReported(t *testing.T) {
	env := newSensorEnv(t)
	blockConfigFile(t, env.cfg.Path())
	code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok, map[string]any{"id": "shed", "kind": "push"})
	if code != http.StatusInternalServerError || !contains(obj["error"], "could not be saved") {
		t.Fatalf("add with an unwritable config file: %d %v", code, obj)
	}
}

// ------------------------------------------------------------------------------------ helpers

func attachFor(t *testing.T, cs config.Sensors, sensor string) config.SensorAttach {
	t.Helper()
	for _, a := range cs.Attach {
		if a.Sensor == sensor {
			return a
		}
	}
	t.Fatalf("no attachment for %q in %+v", sensor, cs.Attach)
	return config.SensorAttach{}
}

func hasString(list []string, want string) bool { return slices.Contains(list, want) }

// contains reports whether a JSON string value mentions want.
func contains(v any, want string) bool {
	s, _ := v.(string)
	return strings.Contains(s, want)
}

// Two edits arriving together must both survive: each handler reads the section, changes it and
// saves it, so without serialising them the second would start from the old section and drop the
// first one's sensor.
func TestConcurrentSensorAddsAllSurvive(t *testing.T) {
	env := newSensorEnv(t)
	tok := env.tok

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := map[string]any{"id": fmt.Sprintf("s%d", i), "kind": "push"}
			codes[i], _, _ = call(t, env.srv, "POST", "/api/v1/sensors", tok, body)
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusCreated {
			t.Fatalf("add s%d: %d", i, code)
		}
	}

	code, obj, _ := call(t, env.srv, "GET", "/api/v1/sensors", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	got := map[string]bool{}
	for _, raw := range obj["sensors"].([]any) {
		got[raw.(map[string]any)["id"].(string)] = true
	}
	for i := range n {
		if id := fmt.Sprintf("s%d", i); !got[id] {
			t.Errorf("%s was lost: %d of %d sensors survived", id, len(got), n)
		}
	}
	// And the file on disk agrees with what the API reports.
	if saved := env.reload(t); len(saved.Sources) != len(got) {
		t.Errorf("config file holds %d sensors, the API reports %d", len(saved.Sources), len(got))
	}
}

// The list says whether nodes on this machine can be given sensors at all, so the GUI can warn
// before someone attaches one and waits for telemetry that will never come.
func TestListSaysWhetherNodesCanCarrySensors(t *testing.T) {
	env := newSensorEnv(t)
	code, obj, _ := call(t, env.srv, "GET", "/api/v1/sensors", env.tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	can, ok := obj["can_publish"].(bool)
	if !ok {
		t.Fatalf("can_publish missing from %v", obj)
	}
	// This build either carries a shim for the machine running the tests or it doesn't; either way
	// the answer and its explanation must agree.
	if why, hasWhy := obj["cannot_publish_why"]; can == hasWhy {
		t.Errorf("can_publish=%v with reason %v: one of them is wrong", can, why)
	}
}

// Saving the Configuration page must not drop the sensors section. The page doesn't know about
// sensors and sends only its own sections, so anything it leaves out has to survive the save —
// this happened on a live site: a relay-role change quietly deleted a working sensor.
func TestConfigSaveKeepsSensors(t *testing.T) {
	env := newSensorEnv(t)
	code, _, _ := call(t, env.srv, "POST", "/api/v1/sensors", env.tok,
		map[string]any{"id": "pole-cpu", "kind": "exec", "command": "echo temperature=44.5", "interval": "1m"})
	if code != http.StatusCreated {
		t.Fatalf("add sensor: %d", code)
	}
	if saved := env.reload(t); len(saved.Sources) != 1 {
		t.Fatalf("the sensor didn't reach the file: %+v", saved)
	}

	// The top bar's relay-role control, which is what was used on the live site.
	code, obj, _ := call(t, env.srv, "PUT", "/api/v1/relay", env.tok, map[string]any{"role": "client_mute"})
	if code != http.StatusOK {
		t.Fatalf("put relay: %d %v", code, obj)
	}
	if saved := env.reload(t); len(saved.Sources) != 1 {
		t.Fatalf("changing the relay role deleted the sensors section: %+v", saved)
	}

	// What the Configuration page sends when someone changes the relay role: its own sections only.
	code, obj, _ = call(t, env.srv, "PUT", "/api/v1/config", env.tok,
		map[string]any{"relay": map[string]any{"role": "client_mute", "long_name": "The Pole Relay", "short_name": "POLE"}})
	if code != http.StatusOK {
		t.Fatalf("save config: %d %v", code, obj)
	}

	saved := env.reload(t)
	if len(saved.Sources) != 1 || saved.Sources[0].ID != "pole-cpu" {
		t.Fatalf("saving the Configuration page deleted the sensors section: %+v", saved)
	}
	code, list, _ := call(t, env.srv, "GET", "/api/v1/sensors", env.tok, nil)
	if code != http.StatusOK || len(list["sensors"].([]any)) != 1 {
		t.Fatalf("the running server lost the sensor too: %d %v", code, list)
	}
}

// The Add dialog's Test button: run a sensor that hasn't been saved, and prove it saves nothing.
func TestTrySensorBeforeSaving(t *testing.T) {
	env := newSensorEnv(t)

	code, obj, _ := call(t, env.srv, "POST", "/api/v1/sensors/try", env.tok,
		map[string]any{"id": "draft", "kind": "exec", "command": "printf 'temperature=8.25\\nhumidity=71\\n'", "interval": "1m"})
	if code != http.StatusOK {
		t.Fatalf("try: %d %v", code, obj)
	}
	rd, _ := obj["reading"].(map[string]any)
	fields, _ := rd["fields"].(map[string]any)
	if fields["temperature"] != 8.25 || fields["humidity"] != float64(71) {
		t.Fatalf("reading = %v", rd)
	}
	if saved := env.reload(t); len(saved.Sources) != 0 {
		t.Errorf("testing a sensor saved it: %+v", saved)
	}
	code, list, _ := call(t, env.srv, "GET", "/api/v1/sensors", env.tok, nil)
	if code != http.StatusOK || len(list["sensors"].([]any)) != 0 {
		t.Errorf("testing a sensor registered it: %v", list)
	}

	// A command that fails says why, in the words the command used.
	code, obj, _ = call(t, env.srv, "POST", "/api/v1/sensors/try", env.tok,
		map[string]any{"id": "draft", "kind": "exec", "command": "echo nope >&2; exit 3", "interval": "1m"})
	if code != http.StatusBadRequest || !strings.Contains(obj["error"].(string), "nope") {
		t.Fatalf("failing command: %d %v", code, obj)
	}
	// And a sensor can't be called after an endpoint that lives under /sensors.
	for _, id := range []string{"try", "interval"} {
		code, _, _ = call(t, env.srv, "POST", "/api/v1/sensors", env.tok, map[string]any{"id": id, "kind": "push"})
		if code != http.StatusBadRequest {
			t.Errorf("a sensor called %q was accepted (%d)", id, code)
		}
	}
}

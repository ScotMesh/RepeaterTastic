// Sensor endpoints: the host's sensors, the readings the GUI shows, and which identities publish
// each one. Every change is written to the config file as well as the registry, so the YAML
// `sensors:` section and the GUI are the same thing (docs/sensors.md, docs/api.md § Sensors).

package web

import (
	"context"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/nodes"
	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
)

// defaultHistoryWindow is how much history GET /sensors/{id}/history returns without ?since=.
const defaultHistoryWindow = time.Hour

func (s *Server) sensorRoutes(priv func(string, http.HandlerFunc)) {
	priv("GET /api/v1/sensors", s.listSensors)
	priv("POST /api/v1/sensors", s.addSensor)
	priv("PUT /api/v1/sensors/interval", s.putSensorInterval)
	priv("PUT /api/v1/sensors/{id}", s.updateSensor)
	priv("DELETE /api/v1/sensors/{id}", s.deleteSensor)
	priv("POST /api/v1/sensors/{id}/read", s.readSensorNow)
	priv("POST /api/v1/sensors/{id}/push", s.pushSensor)
	priv("GET /api/v1/sensors/{id}/history", s.sensorHistory)
	priv("GET /api/v1/identities/{id}/sensors", s.getIdentitySensors)
	priv("PUT /api/v1/identities/{id}/sensors", s.putIdentitySensors)
}

// sensorRegistry is the registry, or nil after answering 503: sensors are a feature the host can be
// without, exactly as plugins are.
func (s *Server) sensorRegistry(w http.ResponseWriter) *sensors.Registry {
	if s.opt.Sensors == nil {
		writeError(w, http.StatusServiceUnavailable, "sensors are turned off on this host")
	}
	return s.opt.Sensors
}

// ------------------------------------------------------------------------------------ JSON

// sensorJSON is one sensor as docs/api.md § Sensors describes it. Durations go out as Go durations
// ("5m0s"), not nanoseconds, so what the GUI reads is what it can send back.
func (s *Server) sensorJSON(st sensors.Status, publishers map[string][]string) map[string]any {
	ids := publishers[st.ID]
	if ids == nil {
		ids = []string{}
	}
	out := map[string]any{
		"id": st.ID, "name": st.Name, "kind": string(st.Kind),
		"last": readingJSON(st.Last), "age_ms": st.AgeMs, "fresh": st.Fresh,
		"reads": st.Reads, "errors": st.Errors, "error": st.Err,
		"identities": ids,
	}
	if st.Command != "" {
		out["command"] = st.Command
	}
	if st.Path != "" {
		out["path"] = st.Path
	}
	if st.Interval > 0 {
		out["interval"] = st.Interval.String()
	}
	if len(st.Scale) > 0 {
		out["scale"] = st.Scale
	}
	return out
}

// readingJSON is one reading, or null when a sensor has never been read: the GUI shows "no reading
// yet" rather than a value from the year zero.
func readingJSON(rd sensors.Reading) any {
	if rd.At.IsZero() {
		return nil
	}
	fields := rd.Fields
	if fields == nil {
		fields = map[sensors.Field]float64{}
	}
	return map[string]any{"at": rd.At, "fields": fields}
}

// sensorFieldCatalogue is every field a source may report: its unit, and the chip a node will think
// it has. An empty chip means no imitated chip can carry it, so the GUI can grey it out instead of
// offering something that would be refused.
func sensorFieldCatalogue() []map[string]any {
	out := make([]map[string]any, 0, len(sensors.Fields))
	for _, f := range sensors.Fields {
		chip := ""
		for _, c := range sensors.Chips {
			if slices.Contains(c.Fields, f) {
				chip = c.Name
				break
			}
		}
		out = append(out, map[string]any{"field": string(f), "unit": f.Unit(), "chip": chip})
	}
	return out
}

// sensorPublishers is the node ids publishing each sensor, worked out the same way a hosted node
// works out its own sensors.
func (s *Server) sensorPublishers() map[string][]string {
	s.cfgMu.Lock()
	sc := s.cfg.Sensors
	s.cfgMu.Unlock()
	out := map[string][]string{}
	for _, rc := range s.radios {
		for _, id := range rc.host.Identities() {
			for _, a := range sc.For(id.UserCopy().GetShortName(), id.NodeID()) {
				out[a.Sensor] = append(out[a.Sensor], id.NodeID())
			}
		}
	}
	return out
}

// writeSensor answers with one sensor's current state.
func (s *Server) writeSensor(w http.ResponseWriter, code int, id string) {
	pub := s.sensorPublishers()
	for _, st := range s.opt.Sensors.Status() {
		if st.ID == id {
			writeJSON(w, code, s.sensorJSON(st, pub))
			return
		}
	}
	writeError(w, http.StatusNotFound, "no sensor called "+id)
}

// ------------------------------------------------------------------------------------ reading

func (s *Server) listSensors(w http.ResponseWriter, r *http.Request) {
	reg := s.opt.Sensors
	if reg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "sensors": []any{}})
		return
	}
	pub := s.sensorPublishers()
	list := []map[string]any{}
	for _, st := range reg.Status() {
		list = append(list, s.sensorJSON(st, pub))
	}
	s.cfgMu.Lock()
	iv := s.cfg.Sensors.SensorInterval()
	s.cfgMu.Unlock()
	out := map[string]any{"enabled": true, "sensors": list, "interval": iv.String(),
		"fields": sensorFieldCatalogue(), "can_publish": nodes.HasShim()}
	if !nodes.HasShim() {
		// Say it here rather than let someone attach a sensor and wonder why no node ever carries
		// it: the library a node preloads is built per architecture (shim/README.md).
		out["cannot_publish_why"] = "this build carries no I²C shim for " + runtime.GOOS + "/" + runtime.GOARCH +
			", so nodes can't be given sensors on this machine; sensors still read and can be pushed to"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) sensorHistory(w http.ResponseWriter, r *http.Request) {
	reg := s.sensorRegistry(w)
	if reg == nil {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.source(id); !ok {
		writeError(w, http.StatusNotFound, "no sensor called "+id)
		return
	}
	window := defaultHistoryWindow
	if raw := r.URL.Query().Get("since"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "since must be a length of time like 1h or 30m")
			return
		}
		window = d
	}
	out := []any{}
	for _, rd := range reg.History(id, time.Now().Add(-window)) {
		out = append(out, readingJSON(rd))
	}
	writeJSON(w, http.StatusOK, out)
}

// ------------------------------------------------------------------------------------ writing

// sensorReq is the body of POST /sensors and PUT /sensors/{id}.
type sensorReq struct {
	ID       string                    `json:"id"`
	Name     string                    `json:"name"`
	Kind     string                    `json:"kind"`
	Command  string                    `json:"command"`
	Path     string                    `json:"path"`
	Interval string                    `json:"interval"`
	Scale    map[sensors.Field]float64 `json:"scale"`
}

// readSource reads a sensor from the request body. fallbackID is used when the body leaves the id
// out, so an edit needs only the changed fields. It writes the error itself and reports false.
func readSource(w http.ResponseWriter, r *http.Request, fallbackID string) (sensors.Source, bool) {
	var req sensorReq
	if !readJSON(w, r, &req) {
		return sensors.Source{}, false
	}
	src := sensors.Source{
		ID:      strings.TrimSpace(req.ID),
		Name:    strings.TrimSpace(req.Name),
		Kind:    sensors.Kind(strings.ToLower(strings.TrimSpace(req.Kind))),
		Command: strings.TrimSpace(req.Command),
		Path:    strings.TrimSpace(req.Path),
		Scale:   req.Scale,
	}
	if src.ID == "" {
		src.ID = fallbackID
	}
	if iv := strings.TrimSpace(req.Interval); iv != "" {
		d, err := time.ParseDuration(iv)
		if err != nil {
			writeError(w, http.StatusBadRequest, "interval must be a length of time like 5m or 30s, not "+iv)
			return sensors.Source{}, false
		}
		src.Interval = d
	}
	for f := range src.Scale {
		if !f.Known() {
			writeError(w, http.StatusBadRequest, "there is no reading called "+string(f)+
				"; the Sensors page lists the fields a node can publish")
			return sensors.Source{}, false
		}
	}
	if err := src.Validate(); err != nil { // fills the name and the default interval
		writeError(w, http.StatusBadRequest, err.Error())
		return sensors.Source{}, false
	}
	// PUT /sensors/interval is the broadcast interval, so a sensor with that id could never be
	// edited again. Say so now rather than let someone make one.
	if src.ID == "interval" {
		writeError(w, http.StatusBadRequest, `"interval" is used by the broadcast interval setting; give the sensor another id`)
		return sensors.Source{}, false
	}
	return src, true
}

func (s *Server) addSensor(w http.ResponseWriter, r *http.Request) {
	if s.sensorRegistry(w) == nil {
		return
	}
	s.sensorMu.Lock() // one change at a time, from reading the section to saving it
	defer s.sensorMu.Unlock()
	src, ok := readSource(w, r, "")
	if !ok {
		return
	}
	s.cfgMu.Lock()
	for _, ex := range s.cfg.Sensors.Sources {
		if strings.EqualFold(ex.ID, src.ID) {
			s.cfgMu.Unlock()
			writeError(w, http.StatusConflict, "a sensor called "+src.ID+" already exists; edit that one, or choose another id")
			return
		}
	}
	next := s.cfg.Sensors
	next.Sources = append(slices.Clone(next.Sources), src)
	s.cfgMu.Unlock()
	if err := s.applySensors(next); err != nil {
		writeStatusError(w, err)
		return
	}
	s.publishSensor(src.ID)
	s.writeSensor(w, http.StatusCreated, src.ID)
}

func (s *Server) updateSensor(w http.ResponseWriter, r *http.Request) {
	if s.sensorRegistry(w) == nil {
		return
	}
	s.sensorMu.Lock() // one change at a time, from reading the section to saving it
	defer s.sensorMu.Unlock()
	id := r.PathValue("id")
	src, ok := readSource(w, r, id)
	if !ok {
		return
	}
	s.cfgMu.Lock()
	next := s.cfg.Sensors
	next.Sources = slices.Clone(next.Sources)
	at := slices.IndexFunc(next.Sources, func(x sensors.Source) bool { return x.ID == id })
	if at < 0 {
		s.cfgMu.Unlock()
		writeError(w, http.StatusNotFound, "no sensor called "+id+" to edit; add it first")
		return
	}
	if src.ID != id {
		for _, ex := range next.Sources {
			if ex.ID != id && strings.EqualFold(ex.ID, src.ID) {
				s.cfgMu.Unlock()
				writeError(w, http.StatusConflict, "a sensor called "+src.ID+" already exists; choose another id")
				return
			}
		}
	}
	next.Sources[at] = src
	// A rename has to follow into the attachments, or they would name a sensor that no longer
	// exists and the config file would stop loading.
	if src.ID != id {
		next.Attach = slices.Clone(next.Attach)
		for i := range next.Attach {
			if next.Attach[i].Sensor == id {
				next.Attach[i].Sensor = src.ID
			}
		}
	}
	s.cfgMu.Unlock()
	if err := s.applySensors(next); err != nil {
		writeStatusError(w, err)
		return
	}
	if src.ID != id {
		s.publishSensor(id) // the old id is gone from the GUI's list
	}
	s.publishSensor(src.ID)
	s.writeSensor(w, http.StatusOK, src.ID)
}

func (s *Server) deleteSensor(w http.ResponseWriter, r *http.Request) {
	if s.sensorRegistry(w) == nil {
		return
	}
	s.sensorMu.Lock() // one change at a time, from reading the section to saving it
	defer s.sensorMu.Unlock()
	id := r.PathValue("id")
	s.cfgMu.Lock()
	next := s.cfg.Sensors
	next.Sources = slices.Clone(next.Sources)
	at := slices.IndexFunc(next.Sources, func(x sensors.Source) bool { return x.ID == id })
	if at < 0 {
		s.cfgMu.Unlock()
		writeError(w, http.StatusNotFound, "no sensor called "+id+" to remove")
		return
	}
	next.Sources = slices.Delete(next.Sources, at, at+1)
	// Deleting a sensor detaches it everywhere: an attachment to a sensor that isn't there stops
	// the config file loading.
	attach := []config.SensorAttach{}
	for _, a := range next.Attach {
		if a.Sensor != id {
			attach = append(attach, a)
		}
	}
	next.Attach = attach
	s.cfgMu.Unlock()
	if err := s.applySensors(next); err != nil {
		writeStatusError(w, err)
		return
	}
	s.publishSensor(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) readSensorNow(w http.ResponseWriter, r *http.Request) {
	reg := s.sensorRegistry(w)
	if reg == nil {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.source(id); !ok {
		writeError(w, http.StatusNotFound, "no sensor called "+id)
		return
	}
	if _, err := reg.ReadNow(r.Context(), id); err != nil {
		// The sensor's own state is worth seeing even when the read failed, but the message is
		// what the operator came for: it's what the command or the file actually said.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeSensor(w, http.StatusOK, id)
}

// pushSensor is POST /sensors/{id}/push: a flat object of field names to numbers. Anything we can't
// carry is ignored, so a source can send its whole reading and let us pick.
func (s *Server) pushSensor(w http.ResponseWriter, r *http.Request) {
	reg := s.sensorRegistry(w)
	if reg == nil {
		return
	}
	id := r.PathValue("id")
	src, ok := s.source(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no sensor called "+id)
		return
	}
	if src.Kind != sensors.Push {
		writeError(w, http.StatusBadRequest, "sensor "+id+" is kind "+string(src.Kind)+
			", so it reads for itself; only a push sensor takes pushed readings")
		return
	}
	var body map[string]any
	if !readJSON(w, r, &body) {
		return
	}
	fields := map[sensors.Field]float64{}
	for k, v := range body {
		n, isNum := v.(float64)
		if f := sensors.Field(strings.ToLower(strings.TrimSpace(k))); isNum && f.Known() {
			fields[f] = n
		}
	}
	if len(fields) == 0 {
		writeError(w, http.StatusBadRequest, "no readings we can carry: send fields named as Meshtastic names them, "+
			"such as temperature or humidity, with a number each")
		return
	}
	if err := reg.Push(id, fields); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeSensor(w, http.StatusOK, id)
}

// putSensorInterval is PUT /sensors/interval: how often a hosted node broadcasts its telemetry.
func (s *Server) putSensorInterval(w http.ResponseWriter, r *http.Request) {
	if s.sensorRegistry(w) == nil {
		return
	}
	s.sensorMu.Lock() // one change at a time, from reading the section to saving it
	defer s.sensorMu.Unlock()
	var req struct {
		Interval string `json:"interval"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var iv time.Duration
	if raw := strings.TrimSpace(req.Interval); raw != "" && raw != "0" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "interval must be a length of time like 1h or 45m, not "+raw)
			return
		}
		iv = d
	}
	if iv < 0 || (iv > 0 && iv < 30*time.Minute) {
		writeError(w, http.StatusBadRequest,
			"broadcast no more often than every 30m: the firmware ignores anything shorter, and a mast full of identities would flood the channel")
		return
	}
	s.cfgMu.Lock()
	next := s.cfg.Sensors
	next.Interval = iv
	s.cfgMu.Unlock()
	if err := s.applySensors(next); err != nil {
		writeStatusError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": next.SensorInterval().String()})
}

// source finds a configured sensor by id.
func (s *Server) source(id string) (sensors.Source, bool) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.cfg.Sensors.Source(id)
}

// applySensors writes a changed sensors section to the config file and hands it to the running
// daemon at once: the registry samples the new sources, and hosted nodes learn who publishes what.
// One source of truth — the YAML section and the GUI are the same thing. Nothing changes if the
// section is refused.
func (s *Server) applySensors(next config.Sensors) error {
	s.cfgMu.Lock()
	before := s.cfg.Sensors
	s.cfg.Sensors = next
	s.cfgMu.Unlock()
	if err := s.sensorsChanged(next); err != nil {
		s.cfgMu.Lock()
		s.cfg.Sensors = before
		s.cfgMu.Unlock()
		_ = s.sensorsChanged(before) // put the running host back as it was
		return err
	}
	// Tell the nodes: a sensor that was removed or changed has to stop being published, and a node
	// only notices its sensors when it starts.
	s.syncHostedSensors()
	if err := s.saveIfPath(); err != nil {
		// The change is live; say plainly that it won't survive a restart rather than pretend to
		// have undone it under the running nodes.
		return errStatus(http.StatusInternalServerError, "applied here, but the config file could not be saved: "+err.Error())
	}
	return nil
}

// sensorsChanged hands a sensors section to the daemon. Without one wired up (a web server on its
// own, in tests) the registry is applied directly, so the GUI still works.
func (s *Server) sensorsChanged(cs config.Sensors) error {
	if s.opt.SensorsChanged != nil {
		return s.opt.SensorsChanged(cs)
	}
	if s.opt.Sensors != nil {
		return s.opt.Sensors.Apply(cs.Sources)
	}
	return nil
}

// ------------------------------------------------------------------------------------ identities

// attachReq is one line of PUT /identities/{id}/sensors.
type attachReq struct {
	Sensor string          `json:"sensor"`
	Fields []sensors.Field `json:"fields"`
}

func (s *Server) getIdentitySensors(w http.ResponseWriter, r *http.Request) {
	if s.sensorRegistry(w) == nil {
		return
	}
	id := s.identityParam(w, r)
	if id == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.attachmentsJSON(id.UserCopy().GetShortName(), id.NodeID()))
}

// attachmentsJSON is what one identity publishes, in the shape the PUT takes back.
func (s *Server) attachmentsJSON(shortName, nodeID string) []map[string]any {
	s.cfgMu.Lock()
	sc := s.cfg.Sensors
	s.cfgMu.Unlock()
	out := []map[string]any{}
	for _, a := range sc.For(shortName, nodeID) {
		fields := []string{}
		for _, f := range a.Fields {
			fields = append(fields, string(f))
		}
		out = append(out, map[string]any{"sensor": a.Sensor, "fields": fields})
	}
	return out
}

// putIdentitySensors rewrites one identity's attachments and bounces its node: meshtasticd only
// looks for sensors when it starts.
func (s *Server) putIdentitySensors(w http.ResponseWriter, r *http.Request) {
	if s.sensorRegistry(w) == nil {
		return
	}
	s.sensorMu.Lock() // one change at a time, from reading the section to saving it
	defer s.sensorMu.Unlock()
	id := s.identityParam(w, r)
	if id == nil {
		return
	}
	var req []attachReq
	if !readJSON(w, r, &req) {
		return
	}
	want, err := s.wantedAttachments(req)
	if err != nil {
		writeStatusError(w, err)
		return
	}
	shortName, nodeID := id.UserCopy().GetShortName(), id.NodeID()

	others := s.otherIdentityIDs(nodeID)
	s.cfgMu.Lock()
	next := s.cfg.Sensors
	next.Attach = setIdentityAttachments(next.Attach, shortName, nodeID, others, want)
	s.cfgMu.Unlock()
	if err := s.applySensors(next); err != nil {
		writeStatusError(w, err)
		return
	}
	for _, a := range want {
		s.publishSensor(a.Sensor)
	}
	// applySensors has already restarted the nodes whose sensors changed, this one included.
	writeJSON(w, http.StatusOK, s.attachmentsJSON(shortName, nodeID))
}

// wantedAttachments checks a requested list and turns it into attachments: a real sensor each, no
// sensor twice, and only fields a node can actually carry.
func (s *Server) wantedAttachments(req []attachReq) ([]sensors.Attachment, error) {
	out := make([]sensors.Attachment, 0, len(req))
	seen := map[string]bool{}
	for _, a := range req {
		a.Sensor = strings.TrimSpace(a.Sensor)
		if _, ok := s.source(a.Sensor); !ok {
			return nil, errStatus(http.StatusBadRequest, "no sensor called "+a.Sensor+" is configured; add it on the Sensors page first")
		}
		if seen[a.Sensor] {
			return nil, errStatus(http.StatusBadRequest, "sensor "+a.Sensor+" is listed twice; give it one line with all its fields")
		}
		seen[a.Sensor] = true
		fields := make([]sensors.Field, 0, len(a.Fields))
		for _, f := range a.Fields {
			switch {
			case !f.Known():
				return nil, errStatus(http.StatusBadRequest, "there is no reading called "+string(f)+
					"; the Sensors page lists the fields a node can publish")
			case !sensors.Carried(f):
				return nil, errStatus(http.StatusBadRequest, "no node can publish "+string(f)+
					": none of the chips RepeaterTastic imitates reports it (see docs/sensors.md)")
			}
			if !slices.Contains(fields, f) {
				fields = append(fields, f)
			}
		}
		if len(fields) == 0 {
			fields = nil // everything the source reports
		}
		out = append(out, sensors.Attachment{Sensor: a.Sensor, Fields: fields})
	}
	return out, nil
}

// otherIdentityIDs is every identity on the host except one, by node id: what "all" has to be
// written out as when one identity's attachments change and the others must keep theirs.
func (s *Server) otherIdentityIDs(except string) []string {
	var out []string
	for _, rc := range s.radios {
		for _, id := range rc.host.Identities() {
			if nid := id.NodeID(); nid != except {
				out = append(out, nid)
			}
		}
	}
	return out
}

// setIdentityAttachments replaces one identity's attachments in list and leaves every other
// identity's alone. An entry naming several identities is split, and "all" is written out as the
// identities it stood for, because the one being edited has to be able to leave it.
func setIdentityAttachments(list []config.SensorAttach, shortName, nodeID string, others []string, want []sensors.Attachment) []config.SensorAttach {
	names := func(a config.SensorAttach) []string {
		var kept []string
		for _, n := range a.Identities {
			n = strings.TrimSpace(n)
			switch {
			case strings.EqualFold(n, "all"):
				for _, o := range others {
					if !slices.Contains(kept, o) {
						kept = append(kept, o)
					}
				}
			case strings.EqualFold(n, nodeID), shortName != "" && strings.EqualFold(n, shortName):
				// This is the identity being edited: what it publishes is said below.
			case !slices.Contains(kept, n):
				kept = append(kept, n)
			}
		}
		return kept
	}
	out := []config.SensorAttach{}
	left := slices.Clone(want)
	for _, a := range list {
		// An "all" entry this identity is happy with is left exactly as it is, so a sensor offered
		// to every identity still reaches the ones added later. Only leaving it, or changing its
		// fields, writes "all" out as the identities it stood for.
		if i := indexOfAttachment(left, a); i >= 0 && onlyAllNames(a, shortName, nodeID) {
			left = slices.Delete(left, i, i+1)
			out = append(out, a)
			continue
		}
		if kept := names(a); len(kept) > 0 {
			a.Identities = kept
			out = append(out, a)
		}
	}
	for _, w := range left {
		// Join an entry that already says exactly this, so the YAML stays as short as a person
		// would have written it; otherwise give this identity its own line.
		merged := false
		for i := range out {
			if out[i].Sensor == w.Sensor && slices.Equal(out[i].Fields, w.Fields) {
				out[i].Identities = append(out[i].Identities, nodeID)
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, config.SensorAttach{Sensor: w.Sensor, Identities: []string{nodeID}, Fields: w.Fields})
		}
	}
	return out
}

// onlyAllNames reports whether a names this identity through "all" and not by its own name or id.
func onlyAllNames(a config.SensorAttach, shortName, nodeID string) bool {
	all := false
	for _, n := range a.Identities {
		n = strings.TrimSpace(n)
		switch {
		case strings.EqualFold(n, "all"):
			all = true
		case strings.EqualFold(n, nodeID), shortName != "" && strings.EqualFold(n, shortName):
			return false
		}
	}
	return all
}

// indexOfAttachment finds the wanted attachment a already provides, or -1.
func indexOfAttachment(want []sensors.Attachment, a config.SensorAttach) int {
	for i, w := range want {
		if w.Sensor == a.Sensor && slices.Equal(w.Fields, a.Fields) {
			return i
		}
	}
	return -1
}

// syncHostedSensors brings every radio's nodes into line with the sensors now configured, restarting
// only those whose sensors actually changed.
func (s *Server) syncHostedSensors() {
	for id, x := range s.opt.Hosting {
		if x == nil {
			continue
		}
		if ids := x.SyncSensors(); len(ids) > 0 {
			s.log.Info("sensors changed: nodes restarted to match", "radio", id, "identities", strings.Join(ids, ","))
		}
	}
}

// ------------------------------------------------------------------------------------ events

// publishSensor tells every radio's event stream that a sensor changed, the way plugin events are
// published: sensors belong to the host, not to a radio, so the stream sends them once.
func (s *Server) publishSensor(id string) {
	for _, rc := range s.radios {
		rc.host.Bus.Publish(mesh.Event{Type: "sensor", Data: id})
	}
}

// forwardSensorReads turns every reading (and every failed read) into a sensor event, so a GUI
// watching a sensor sees it come in without polling. One subscription for the whole server, fanned
// out to every radio's stream.
func (s *Server) forwardSensorReads(ctx context.Context) {
	ch, unsub := s.opt.Sensors.Subscribe(64)
	defer unsub()
	s.fanOutSensors(ctx, ch)
}

// fanOutSensors publishes a sensor event for each id that arrives, until ctx ends.
func (s *Server) fanOutSensors(ctx context.Context, ch <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-ch:
			if !ok {
				return
			}
			s.publishSensor(id)
		}
	}
}

// sensorEventPayload is a sensor's new state, or a note that it's gone.
func (s *Server) sensorEventPayload(e mesh.Event) (any, bool) {
	id, _ := e.Data.(string)
	if s.opt.Sensors == nil {
		return nil, false
	}
	pub := s.sensorPublishers()
	for _, st := range s.opt.Sensors.Status() {
		if st.ID == id {
			return s.sensorJSON(st, pub), true
		}
	}
	return map[string]any{"id": id, "deleted": true}, true
}

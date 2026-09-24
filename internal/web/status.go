// Status, relay mode, the event stream and logs.

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/phy"
	"github.com/ScotMesh/RepeaterTastic/internal/wire"
)

func (s *Server) statusJSON(r *http.Request) map[string]any {
	return s.radioStatus(r.Context(), s.radioFor(r))
}

// radioStatus is one radio's status: its radio, PHY, relay, airtime and counters.
func (s *Server) radioStatus(ctx context.Context, rc *radioCtx) map[string]any {
	h := rc.host
	now := time.Now()
	rp := h.RadioParams()
	hc := h.Config()
	info := h.Radio().Info()
	st := rc.stats(ctx)
	txMs, rxMs := h.Air.HourTotals(now)
	duty := hc.DutyCyclePct
	if duty == 0 {
		duty = rp.Region.DutyCyclePct
	}
	if hc.OverrideDutyCycle {
		duty = 100
	}
	primary := hc.PrimaryChannel
	if primary == "" {
		primary = rp.PresetName()
	}
	relay := map[string]any{"role": hc.RelayRole}
	if rel := h.Relay(); rel != nil {
		u := rel.UserCopy()
		relay["node_id"], relay["node_num"], relay["long_name"], relay["short_name"] = rel.NodeID(), rel.NodeNum, u.LongName, u.ShortName
	}
	c := &h.Counters
	return map[string]any{
		"version": s.opt.Version, "uptime_s": int(now.Sub(h.Started()).Seconds()),
		"radio_id": rc.id, "radio_name": rc.name, "site": s.siteJSON(),
		"radio": map[string]any{"driver": info.Driver, "device": s.radioConfig(rc).Radio.Device, "firmware": info.Firmware, "name": info.Name,
			"connected": st.Connected, "configured": h.RadioConfigured(), "reconnects": st.Reconnects, "rx": st.RxPackets,
			"tx": st.TxPackets, "errors": st.Errors, "noise_floor_dbm": st.NoiseFloorDBm, "queue": h.QueueLen()},
		"phy":             phyJSON(rp, primary),
		"relay":           relay,
		"map":             map[string]any{"tile_url": withMapKey(mapTileURL(s.cfg.Web.MapTileURL), s.opt.MapAPIKey)},
		"restart_reasons": s.restartReasons(),
		"nodes":           s.nodesHealth(),
		"airtime": map[string]any{"window_s": 3600, "tx_ms": txMs, "rx_ms": rxMs, "duty_limit_pct": duty,
			"tx_pct": h.Air.TxPercent(now), "channel_util_pct": h.Air.ChannelUtilPercent(now)},
		"counters": map[string]uint64{"rx": c.Rx.Load(), "rx_dupe": c.RxDupe.Load(), "rx_undecryptable": c.RxUndecryptable.Load(),
			"rx_bad": c.RxBad.Load(), "tx": c.Tx.Load(), "tx_failed": c.TxFailed.Load(), "relayed": c.Relayed.Load(),
			"relay_cancelled": c.RelayCancelled.Load(), "ack_ok": c.AckOK.Load(), "ack_fail": c.AckFail.Load(),
			"dropped_duty": c.DroppedDuty.Load()},
	}
}

func phyJSON(rp phy.RadioParams, primary string) map[string]any {
	return map[string]any{"region": rp.Region.Name, "preset": rp.Preset.String(), "preset_name": rp.PresetName(),
		"frequency_mhz": rp.FrequencyMHz, "bw_khz": rp.BwKHz, "sf": rp.SF, "cr": rp.CR, "slot": rp.Slot,
		"num_slots": rp.NumSlots, "sync_word": rp.SyncWord, "preamble": rp.Preamble, "tx_power_dbm": rp.TxPowerDBm,
		"primary_channel": primary, "duty_cycle_pct": rp.Region.DutyCyclePct}
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusJSON(r))
}

func (s *Server) putRelay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if rc := s.radioFor(r); rc != s.radios[0] {
		s.putExtraRelay(w, r, rc, req.Role)
		return
	}
	s.cfgMu.Lock()
	next := *s.cfg
	next.Relay.Role = mesh.NormalizeRelayRole(req.Role)
	s.cfgMu.Unlock()
	if err := s.applyConfig(r, &next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.statusJSON(r)["relay"])
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	writeJSON(w, http.StatusOK, s.opt.Logs.Recent(limit))
}

// radioEvent is a bus event from one of the radios.
type radioEvent struct {
	rc *radioCtx
	e  mesh.Event
}

// events is GET /events: the live stream for one radio, or with ?radio=all for every radio
// (a status event per radio, nodes merged across radios).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set(cacheControl, "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	radios := s.radiosFor(r)
	ch := make(chan radioEvent, 512)
	ctx := r.Context()
	for _, rc := range radios {
		sub, unsub := rc.host.Bus.Subscribe(512)
		defer unsub()
		go forwardEvents(ctx, rc, sub, ch)
	}
	out := sseStream{w: w, fl: fl}
	if !s.sendStatuses(ctx, out, radios) {
		return
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !s.sendStatuses(ctx, out, radios) {
				return
			}
		case re := <-ch:
			payload, ok := s.eventPayload(re, radios)
			if ok && !out.send(re.e.Type, payload) {
				return
			}
		}
	}
}

// forwardEvents passes a radio's bus events into the stream's channel until the request ends.
func forwardEvents(ctx context.Context, rc *radioCtx, sub <-chan mesh.Event, ch chan<- radioEvent) {
	for e := range sub {
		select {
		case ch <- radioEvent{rc, e}:
		case <-ctx.Done():
			return
		}
	}
}

// sseStream writes server-sent events.
type sseStream struct {
	w  http.ResponseWriter
	fl http.Flusher
}

// send writes one event; false means the client has gone. A value that can't be encoded is skipped.
func (o sseStream) send(event string, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(o.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return false
	}
	o.fl.Flush()
	return true
}

// sendStatuses sends a status event per radio; false means the client has gone.
func (s *Server) sendStatuses(ctx context.Context, out sseStream, radios []*radioCtx) bool {
	for _, rc := range radios {
		if !out.send("status", s.radioStatus(ctx, rc)) {
			return false
		}
	}
	return true
}

// eventPayload is what the stream sends for a bus event (false: skip it).
func (s *Server) eventPayload(re radioEvent, radios []*radioCtx) (any, bool) {
	e := re.e
	site := len(radios) > 1
	switch e.Type {
	case "identity":
		return s.identityEventPayload(re, site)
	case "node":
		return s.nodeEventPayload(re, radios)
	case "log", "plugin", "sensor":
		// Published on every radio's bus: send them once.
		if site && re.rc != radios[0] {
			return nil, false
		}
		switch e.Type {
		case "log":
			return e.Data, true
		case "sensor":
			return s.sensorEventPayload(e)
		}
		return s.pluginEventPayload(e)
	}
	return e.Data, true
}

// identityEventPayload is an identity's new state, or a note that it's gone.
func (s *Server) identityEventPayload(re radioEvent, site bool) (any, bool) {
	idStr, _ := re.e.Data.(string)
	num, _ := wire.ParseNodeID(idStr)
	if id := re.rc.host.Identity(num); id != nil {
		return s.identityJSON(id), true
	}
	if site && s.radioHolding(num) != nil {
		return nil, false // it moved to another radio, which announces it
	}
	return map[string]any{"node_id": idStr, "deleted": true}, true
}

// nodeEventPayload is a node's new state: across the site when the stream covers several radios.
func (s *Server) nodeEventPayload(re radioEvent, radios []*radioCtx) (any, bool) {
	idStr, _ := re.e.Data.(string)
	num, _ := wire.ParseNodeID(idStr)
	if len(radios) > 1 {
		if n := s.siteNodes(radios, num); len(n) == 1 {
			return n[0], true
		}
		return nil, false
	}
	en, ok := re.rc.host.DB.Get(num)
	if !ok {
		return nil, false
	}
	n := nodeJSON(en, s.localIDs(re.rc.host))
	n["heard_by"] = heardBy(en, re.rc.id)
	return n, true
}

// pluginEventPayload is a plugin's new state, or a note that it's gone.
func (s *Server) pluginEventPayload(e mesh.Event) (any, bool) {
	id, _ := e.Data.(string)
	if s.opt.Plugins == nil {
		return nil, false
	}
	if in, err := s.opt.Plugins.Get(id); err == nil {
		return s.pluginJSON(in), true
	}
	return map[string]any{"id": id, "deleted": true}, true
}

package sensors

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"
)

// histKeep is how many readings we remember per source: twelve hours at the default one-minute
// interval, which is enough for a sparkline in the GUI without growing without bound.
const histKeep = 720

// pushWindow is how long a pushed reading counts as current. A push source has no interval of its
// own, so we can only pick a window: a quarter of an hour is long enough for a plugin that pushes
// every few minutes, short enough that a dead pusher shows up in the GUI.
const pushWindow = 15 * time.Minute

// Status is one source as the GUI shows it: what it is, its last reading, and how it is behaving.
type Status struct {
	Source `json:",inline"` // flatten in JSON: id, name, kind, ...
	Last   Reading          `json:"last"`
	AgeMs  int64            `json:"age_ms"`
	Fresh  bool             `json:"fresh"` // read within 3× its interval (push: 15m)
	Err    string           `json:"error,omitempty"`
	Reads  uint64           `json:"reads"`
	Errors uint64           `json:"errors"`
}

// entry is one source and everything we have learnt about it. Readings are never edited once
// stored, so a reader may keep a Reading (and its map) after the lock is dropped.
type entry struct {
	src    Source
	last   Reading
	hist   []Reading
	err    string
	reads  uint64
	errors uint64
	// quiet is set once a failure has been logged, so a sensor that is broken all week doesn't
	// write the same line into the log every minute. Cleared when it reads again.
	quiet bool
}

// Registry holds every configured source, samples the ones that need sampling, and keeps the last
// readings for the GUI, the API and the files hosted nodes read.
type Registry struct {
	log *slog.Logger
	// now and maxWait are swapped in tests, so freshness and the command timeout can be checked
	// without waiting in real time.
	now     func() time.Time
	maxWait time.Duration

	mu      sync.RWMutex
	entries map[string]*entry
	ids     []string // sorted, so Sources, Status and the GUI keep a stable order

	subMu sync.Mutex
	subs  map[chan string]struct{}

	// changed wakes Run when the set of sources changes, so an edit in the GUI takes effect now
	// rather than at the next tick.
	changed chan struct{}
}

// New makes an empty registry. Sources arrive from the config file via Apply, or from the GUI.
func New(log *slog.Logger) *Registry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Registry{
		log:     log,
		now:     time.Now,
		maxWait: execCap,
		entries: map[string]*entry{},
		subs:    map[chan string]struct{}{},
		changed: make(chan struct{}, 1),
	}
}

// Apply replaces the whole set, as the config file is loaded or saved. Sources are validated before
// anything changes, so a bad entry leaves the running set alone, and ids that survive keep their
// readings: a config reload mustn't blank the GUI or the files nodes are reading.
func (r *Registry) Apply(sources []Source) error {
	next := make(map[string]*entry, len(sources))
	ids := make([]string, 0, len(sources))
	for _, s := range sources {
		if err := s.Validate(); err != nil { // fills defaults on our copy
			return err
		}
		if _, dup := next[s.ID]; dup {
			return fmt.Errorf("two sensors are called %q; give each one its own id", s.ID)
		}
		next[s.ID] = &entry{src: s.clone()}
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)

	r.mu.Lock()
	for id, e := range next {
		if old, ok := r.entries[id]; ok {
			e.last, e.hist, e.err = old.last, old.hist, old.err
			e.reads, e.errors, e.quiet = old.reads, old.errors, old.quiet
		}
	}
	r.entries, r.ids = next, ids
	r.mu.Unlock()
	r.wake()
	return nil
}

// Add adds one source, for the GUI's "add sensor".
func (r *Registry) Add(s Source) error {
	if err := s.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	if _, ok := r.entries[s.ID]; ok {
		r.mu.Unlock()
		return fmt.Errorf("a sensor called %q already exists; edit that one, or choose another id", s.ID)
	}
	r.entries[s.ID] = &entry{src: s.clone()}
	r.reindexLocked()
	r.mu.Unlock()
	r.wake()
	return nil
}

// Update replaces one source, for the GUI's edit. Readings already taken are kept, and s may carry
// a new id to rename it.
func (r *Registry) Update(id string, s Source) error {
	if err := s.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("no sensor called %q to edit; add it first", id)
	}
	if s.ID != id {
		if _, taken := r.entries[s.ID]; taken {
			r.mu.Unlock()
			return fmt.Errorf("cannot rename sensor %q to %q: a sensor with that id already exists", id, s.ID)
		}
		delete(r.entries, id)
		r.entries[s.ID] = e
	}
	e.src = s.clone()
	r.reindexLocked()
	r.mu.Unlock()
	r.wake()
	return nil
}

// Remove forgets a source and its readings.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	if _, ok := r.entries[id]; !ok {
		r.mu.Unlock()
		return fmt.Errorf("no sensor called %q to remove", id)
	}
	delete(r.entries, id)
	r.reindexLocked()
	r.mu.Unlock()
	r.wake()
	return nil
}

// Sources lists the configured sources in a stable order (by id).
func (r *Registry) Sources() []Source {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Source, 0, len(r.ids))
	for _, id := range r.ids {
		out = append(out, r.entries[id].src.clone())
	}
	return out
}

// Push records a reading sent to us, for kind push: a plugin, a script or the HTTP API.
func (r *Registry) Push(id string, fields map[Field]float64) error {
	r.mu.RLock()
	e, ok := r.entries[id]
	var src Source
	if ok {
		src = e.src // the stored Scale map is never edited in place, so keeping it is safe
	}
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("no sensor called %q; add it with kind push before pushing readings to it", id)
	}
	if src.Kind != Push {
		return fmt.Errorf("sensor %q is kind %s, so it reads for itself; only a push sensor takes pushed readings", id, src.Kind)
	}
	vals := scaleFields(src.Scale, fields)
	if len(vals) == 0 {
		return fmt.Errorf("sensor %q: no readings we can carry; send fields named as Meshtastic names them, such as temperature or humidity", id)
	}
	r.record(id, Reading{At: r.now(), Fields: vals}, nil)
	return nil
}

// Latest returns the last reading for a source, false if it has none yet.
func (r *Registry) Latest(id string) (Reading, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok || e.last.At.IsZero() {
		return Reading{}, false
	}
	return e.last.clone(), true
}

// History returns the readings kept for a source since a moment, oldest first.
func (r *Registry) History(id string, since time.Time) []Reading {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok {
		return nil
	}
	out := make([]Reading, 0, len(e.hist))
	for _, rd := range e.hist {
		if rd.At.After(since) {
			out = append(out, rd.clone())
		}
	}
	return out
}

// Status describes every source, in the same stable order as Sources.
func (r *Registry) Status() []Status {
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Status, 0, len(r.ids))
	for _, id := range r.ids {
		e := r.entries[id]
		st := Status{
			Source: e.src.clone(),
			Last:   e.last.clone(),
			Err:    e.err,
			Reads:  e.reads,
			Errors: e.errors,
		}
		if !e.last.At.IsZero() {
			age := now.Sub(e.last.At)
			if age < 0 { // a clock step backwards shouldn't read as a negative age
				age = 0
			}
			st.AgeMs = age.Milliseconds()
			st.Fresh = age < staleAfter(e.src)
		}
		out = append(out, st)
	}
	return out
}

// Subscribe hands back a channel of source ids, one each time a reading lands, and a function to
// stop listening. Sends never block a sampling loop: a listener that isn't keeping up misses ids.
func (r *Registry) Subscribe(buf int) (<-chan string, func()) {
	ch := make(chan string, buf)
	r.subMu.Lock()
	r.subs[ch] = struct{}{}
	r.subMu.Unlock()
	return ch, func() {
		r.subMu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.subMu.Unlock()
	}
}

// record stores the outcome of one read: the counters, the reading or the error, and a nudge to
// anyone watching.
func (r *Registry) record(id string, rd Reading, readErr error) {
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok { // removed while a read was in flight
		r.mu.Unlock()
		return
	}
	var logFailure, logRecovery bool
	if readErr != nil {
		e.errors++
		e.err = readErr.Error()
		if !e.quiet {
			e.quiet, logFailure = true, true
		}
	} else {
		e.reads++
		e.err = ""
		if e.quiet {
			e.quiet, logRecovery = false, true
		}
		e.last = rd
		e.hist = append(e.hist, rd)
		if len(e.hist) > histKeep {
			e.hist = e.hist[len(e.hist)-histKeep:]
		}
	}
	r.mu.Unlock()

	switch {
	case logFailure:
		r.log.Warn("sensor read failed", "sensor", id, "error", readErr)
	case logRecovery:
		r.log.Info("sensor is reading again", "sensor", id)
	}
	if readErr == nil {
		r.publish(id)
	}
}

// publish tells subscribers a reading landed, dropping the id for anyone whose buffer is full.
func (r *Registry) publish(id string) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- id:
		default:
		}
	}
}

// wake nudges Run that the set of sources changed. Never blocks: one pending nudge is enough.
func (r *Registry) wake() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// reindexLocked rebuilds the sorted id list. Callers hold mu for writing.
func (r *Registry) reindexLocked() {
	ids := make([]string, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	r.ids = ids
}

// staleAfter is how long a reading stays fresh: three intervals, so one missed read doesn't turn
// the GUI red, and a fixed window for a push source, which has no interval to go by.
func staleAfter(s Source) time.Duration {
	if s.Kind == Push || s.Interval <= 0 {
		return pushWindow
	}
	return 3 * s.Interval
}

// scaleFields keeps only fields we can carry and applies the source's multipliers, so everything
// downstream sees the units Meshtastic expects. A value that isn't a number is dropped too: the
// file the shim reads must hold a number, and a source printing "nan" shouldn't reach it.
func scaleFields(scale map[Field]float64, in map[Field]float64) map[Field]float64 {
	out := make(map[Field]float64, len(in))
	for f, v := range in {
		if !f.Known() {
			continue
		}
		if m, ok := scale[f]; ok {
			v *= m
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out[f] = v
	}
	return out
}

// clone copies a source deeply, so the caller and the registry can't edit each other's Scale.
func (s Source) clone() Source {
	out := s
	if s.Scale != nil {
		out.Scale = make(map[Field]float64, len(s.Scale))
		for f, m := range s.Scale {
			out.Scale[f] = m
		}
	}
	return out
}

// clone copies a reading deeply, so a caller can't edit what the registry is holding.
func (rd Reading) clone() Reading {
	out := Reading{At: rd.At}
	if rd.Fields != nil {
		out.Fields = make(map[Field]float64, len(rd.Fields))
		for f, v := range rd.Fields {
			out.Fields[f] = v
		}
	}
	return out
}

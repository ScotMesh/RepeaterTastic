package sensors

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock stands in for time.Now, so freshness and ages can be checked without waiting.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// testReg is a registry on a clock we control and a log nobody reads.
func testReg(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	clk := &fakeClock{at: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	r := New(nil)
	r.now = clk.Now
	return r, clk
}

// statusByID indexes Status for assertions that don't care about order.
func statusByID(r *Registry) map[string]Status {
	out := map[string]Status{}
	for _, st := range r.Status() {
		out[st.ID] = st
	}
	return out
}

func mustApply(t *testing.T, r *Registry, sources ...Source) {
	t.Helper()
	if err := r.Apply(sources); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestApplyValidationLeavesTheSetAlone(t *testing.T) {
	cases := []struct {
		name string
		src  Source
		want string
	}{
		{"no id", Source{Kind: Push}, "needs an id"},
		{"space in id", Source{ID: "out side", Kind: Push}, "letters, digits, dashes"},
		{"exec without a command", Source{ID: "cpu", Kind: Exec}, "needs a command to run"},
		{"file without a path", Source{ID: "rain", Kind: File}, "needs a path to read"},
		{"unknown kind", Source{ID: "cpu", Kind: "magic"}, "must be push, exec or file"},
		{"read too often", Source{ID: "cpu", Kind: Exec, Command: "true", Interval: time.Second}, "no more often than every 5s"},
		{"unknown scale field", Source{ID: "cpu", Kind: Push, Scale: map[Field]float64{"windspeed": 2}}, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := testReg(t)
			mustApply(t, r, Source{ID: "keep", Kind: Push})
			err := r.Apply([]Source{tc.src})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Apply error = %v, want one mentioning %q", err, tc.want)
			}
			if got := r.Sources(); len(got) != 1 || got[0].ID != "keep" {
				t.Fatalf("a rejected source changed the set: %+v", got)
			}
		})
	}
}

func TestApplyRejectsTwoOfTheSameID(t *testing.T) {
	r, _ := testReg(t)
	err := r.Apply([]Source{{ID: "cpu", Kind: Push}, {ID: "cpu", Kind: Push}})
	if err == nil || !strings.Contains(err.Error(), "two sensors are called") {
		t.Fatalf("Apply error = %v, want one about a repeated id", err)
	}
}

func TestApplyFillsDefaultsAndSortsByID(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r,
		Source{ID: "zulu", Kind: Exec, Command: "true"},
		Source{ID: "alpha", Kind: Push, Name: "Rain gauge"},
	)
	got := r.Sources()
	if len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "zulu" {
		t.Fatalf("Sources not sorted by id: %+v", got)
	}
	if got[1].Interval != time.Minute {
		t.Errorf("exec interval = %s, want the 1m default", got[1].Interval)
	}
	if got[1].Name != "zulu" {
		t.Errorf("name = %q, want the id as a fallback", got[1].Name)
	}
	if got[0].Name != "Rain gauge" {
		t.Errorf("name = %q, want the configured name", got[0].Name)
	}
}

func TestApplyKeepsReadingsForSurvivingIDs(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r, Source{ID: "keep", Kind: Push}, Source{ID: "drop", Kind: Push})
	if err := r.Push("keep", map[Field]float64{Temperature: 21}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := r.Push("drop", map[Field]float64{Temperature: 5}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	mustApply(t, r, Source{ID: "keep", Kind: Push, Name: "renamed"}, Source{ID: "new", Kind: Push})
	if rd, ok := r.Latest("keep"); !ok || rd.Fields[Temperature] != 21 {
		t.Fatalf("Latest(keep) = %+v, %v; want the reading kept across Apply", rd, ok)
	}
	if st := statusByID(r)["keep"]; st.Reads != 1 || st.Name != "renamed" {
		t.Errorf("keep status = %+v; want reads kept and the new name", st)
	}
	if _, ok := r.Latest("drop"); ok {
		t.Error("a removed sensor still has a reading")
	}
	if _, ok := r.Latest("new"); ok {
		t.Error("a new sensor already has a reading")
	}
	if len(r.History("drop", time.Time{})) != 0 {
		t.Error("a removed sensor still has history")
	}
}

func TestAddUpdateRemove(t *testing.T) {
	r, _ := testReg(t)
	if err := r.Add(Source{ID: "cpu", Kind: Exec, Command: "true"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := r.Add(Source{ID: "cpu", Kind: Push}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Add of a repeated id = %v, want an error saying so", err)
	}
	if err := r.Add(Source{ID: "cpu2", Kind: Exec}); err == nil {
		t.Fatal("Add accepted an exec source with no command")
	}

	if err := r.Update("nope", Source{ID: "nope", Kind: Push}); err == nil || !strings.Contains(err.Error(), "no sensor called") {
		t.Fatalf("Update of an unknown id = %v, want an error saying so", err)
	}
	if err := r.Update("cpu", Source{ID: "cpu", Kind: Exec}); err == nil {
		t.Fatal("Update accepted an exec source with no command")
	}
	if err := r.Update("cpu", Source{ID: "cpu", Kind: Exec, Command: "true", Interval: 30 * time.Second}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := r.Sources()[0].Interval; got != 30*time.Second {
		t.Errorf("interval after Update = %s, want 30s", got)
	}

	// A rename carries the readings over, and can't land on an id already in use.
	if err := r.Add(Source{ID: "rain", Kind: Push}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := r.Push("rain", map[Field]float64{Rainfall1h: 2}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := r.Update("rain", Source{ID: "cpu", Kind: Push}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("rename onto a used id = %v, want an error saying so", err)
	}
	if err := r.Update("rain", Source{ID: "gauge", Kind: Push}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if rd, ok := r.Latest("gauge"); !ok || rd.Fields[Rainfall1h] != 2 {
		t.Errorf("Latest(gauge) = %+v, %v; want the reading carried over by the rename", rd, ok)
	}
	if _, ok := r.Latest("rain"); ok {
		t.Error("the old id still has readings after a rename")
	}

	if err := r.Remove("nope"); err == nil || !strings.Contains(err.Error(), "no sensor called") {
		t.Fatalf("Remove of an unknown id = %v, want an error saying so", err)
	}
	if err := r.Remove("cpu"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := r.Sources(); len(got) != 1 || got[0].ID != "gauge" {
		t.Fatalf("Sources after Remove = %+v", got)
	}
}

func TestPush(t *testing.T) {
	r, clk := testReg(t)
	mustApply(t, r,
		Source{ID: "rain", Kind: Push, Scale: map[Field]float64{Rainfall1h: 25.4}},
		Source{ID: "cpu", Kind: Exec, Command: "true"},
	)

	if err := r.Push("nope", map[Field]float64{Temperature: 1}); err == nil || !strings.Contains(err.Error(), "no sensor called") {
		t.Fatalf("Push to an unknown id = %v, want an error saying so", err)
	}
	if err := r.Push("cpu", map[Field]float64{Temperature: 1}); err == nil || !strings.Contains(err.Error(), "only a push sensor") {
		t.Fatalf("Push to an exec sensor = %v, want an error saying so", err)
	}
	if err := r.Push("rain", map[Field]float64{"windspeed": 4}); err == nil || !strings.Contains(err.Error(), "no readings we can carry") {
		t.Fatalf("Push of unknown fields = %v, want an error saying so", err)
	}

	// Scale is applied on arrival, and a field we don't carry is dropped rather than stored.
	if err := r.Push("rain", map[Field]float64{Rainfall1h: 2, "windspeed": 4}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	rd, ok := r.Latest("rain")
	if !ok {
		t.Fatal("no reading after Push")
	}
	if got := rd.Fields[Rainfall1h]; got != 50.8 {
		t.Errorf("rainfall_1h = %v, want 2 × 25.4", got)
	}
	if _, bad := rd.Fields["windspeed"]; bad {
		t.Error("an unknown field was stored")
	}
	if !rd.At.Equal(clk.Now()) {
		t.Errorf("reading time = %s, want now", rd.At)
	}
	if st := statusByID(r)["rain"]; st.Reads != 1 || st.Errors != 0 || st.Err != "" {
		t.Errorf("status = %+v, want one clean read", st)
	}
}

func TestLatestUnknownAndUnread(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r, Source{ID: "rain", Kind: Push})
	if _, ok := r.Latest("nope"); ok {
		t.Error("Latest of an unknown sensor said yes")
	}
	if _, ok := r.Latest("rain"); ok {
		t.Error("Latest of an unread sensor said yes")
	}
	if st := statusByID(r)["rain"]; st.Fresh || st.AgeMs != 0 {
		t.Errorf("status of an unread sensor = %+v, want no age and not fresh", st)
	}
}

func TestFreshness(t *testing.T) {
	r, clk := testReg(t)
	mustApply(t, r,
		Source{ID: "cpu", Kind: Exec, Command: "true", Interval: 10 * time.Second},
		Source{ID: "rain", Kind: Push},
	)
	r.record("cpu", Reading{At: clk.Now(), Fields: map[Field]float64{Temperature: 20}}, nil)
	if err := r.Push("rain", map[Field]float64{Rainfall1h: 1}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	st := statusByID(r)
	if !st["cpu"].Fresh || !st["rain"].Fresh {
		t.Fatalf("a just-read sensor isn't fresh: %+v", st)
	}

	clk.advance(29 * time.Second) // still inside 3 × 10s
	st = statusByID(r)
	if !st["cpu"].Fresh || st["cpu"].AgeMs != 29000 {
		t.Errorf("cpu at 29s = %+v, want fresh with age 29000", st["cpu"])
	}

	clk.advance(2 * time.Second) // past 3 × 10s
	st = statusByID(r)
	if st["cpu"].Fresh {
		t.Error("cpu is still fresh three intervals later")
	}
	if !st["rain"].Fresh {
		t.Error("a push reading went stale inside 15m")
	}

	clk.advance(15 * time.Minute)
	if statusByID(r)["rain"].Fresh {
		t.Error("a push reading is still fresh after 15m")
	}
}

func TestStatusCountsErrorsAndKeepsOrder(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r, Source{ID: "b", Kind: Push}, Source{ID: "a", Kind: Push})
	r.record("a", Reading{}, errNo("i2c bus is busy"))
	r.record("a", Reading{}, errNo("i2c bus is busy"))

	all := r.Status()
	if len(all) != 2 || all[0].ID != "a" || all[1].ID != "b" {
		t.Fatalf("Status order = %+v, want a then b", all)
	}
	if all[0].Errors != 2 || all[0].Reads != 0 || !strings.Contains(all[0].Err, "i2c bus is busy") {
		t.Fatalf("status after two failures = %+v", all[0])
	}

	// A good read clears the error the GUI is showing.
	if err := r.Push("a", map[Field]float64{Temperature: 1}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if st := statusByID(r)["a"]; st.Err != "" || st.Reads != 1 || st.Errors != 2 {
		t.Errorf("status after a good read = %+v, want the error cleared and the count kept", st)
	}
}

func TestSubscribeFansOutAndDropsWhenFull(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r, Source{ID: "rain", Kind: Push})

	one, stopOne := r.Subscribe(4)
	two, stopTwo := r.Subscribe(4)
	defer stopOne()

	if err := r.Push("rain", map[Field]float64{Rainfall1h: 1}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	for i, ch := range []<-chan string{one, two} {
		select {
		case id := <-ch:
			if id != "rain" {
				t.Errorf("subscriber %d got %q, want rain", i, id)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d heard nothing", i)
		}
	}

	stopTwo()
	if _, open := <-two; open {
		t.Error("the channel wasn't closed when the subscriber stopped")
	}
	if err := r.Push("rain", map[Field]float64{Rainfall1h: 2}); err != nil {
		t.Fatalf("Push after a subscriber left: %v", err)
	}
	if id := <-one; id != "rain" {
		t.Errorf("remaining subscriber got %q", id)
	}

	// A subscriber that isn't reading misses ids; it must never hold up a read.
	full, stopFull := r.Subscribe(1)
	defer stopFull()
	for i := 0; i < 3; i++ {
		if err := r.Push("rain", map[Field]float64{Rainfall1h: float64(i)}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	if got := len(full); got != 1 {
		t.Errorf("queued ids = %d, want 1 kept and the rest dropped", got)
	}
	if st := statusByID(r)["rain"]; st.Reads != 5 {
		t.Errorf("reads = %d, want 5: a full subscriber mustn't lose a reading", st.Reads)
	}
}

func TestHistoryWindowCapAndCopies(t *testing.T) {
	r, clk := testReg(t)
	mustApply(t, r, Source{ID: "rain", Kind: Push})
	start := clk.Now()
	for i := 0; i < 5; i++ {
		clk.advance(time.Minute)
		if err := r.Push("rain", map[Field]float64{Temperature: float64(i)}); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	if got := r.History("rain", start); len(got) != 5 {
		t.Fatalf("History since the start = %d readings, want 5", len(got))
	}
	if got := r.History("rain", start.Add(3*time.Minute)); len(got) != 2 {
		t.Errorf("History since +3m = %d readings, want 2", len(got))
	}
	if got := r.History("nope", time.Time{}); got != nil {
		t.Errorf("History of an unknown sensor = %+v, want nil", got)
	}

	// The registry hands out copies: editing one mustn't change what it holds.
	got := r.History("rain", start)
	got[0].Fields[Temperature] = 999
	if again := r.History("rain", start); again[0].Fields[Temperature] == 999 {
		t.Error("History returned the registry's own map")
	}

	for i := 0; i < histKeep+20; i++ {
		clk.advance(time.Minute)
		if err := r.Push("rain", map[Field]float64{Temperature: float64(1000 + i)}); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	kept := r.History("rain", time.Time{})
	if len(kept) != histKeep {
		t.Fatalf("history length = %d, want it capped at %d", len(kept), histKeep)
	}
	if want := float64(1000 + 20); kept[0].Fields[Temperature] != want {
		t.Errorf("oldest kept reading = %v, want %v: the cap should drop the oldest", kept[0].Fields[Temperature], want)
	}
}

// errNo is a tiny error for recording failures in tests.
type errNo string

func (e errNo) Error() string { return string(e) }

func TestStatusJSONIsFlat(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r, Source{ID: "rain", Kind: Push, Name: "Rain gauge"})
	if err := r.Push("rain", map[Field]float64{Rainfall1h: 1}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	b, err := json.Marshal(r.Status()[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// The GUI reads the source's own fields at the top level, beside the reading.
	for _, key := range []string{"id", "name", "kind", "last", "age_ms", "fresh", "reads", "errors"} {
		if _, ok := got[key]; !ok {
			t.Errorf("status JSON has no %q: %s", key, b)
		}
	}
	if _, nested := got["Source"]; nested {
		t.Errorf("status JSON nests the source: %s", b)
	}
	if got["error"] != nil {
		t.Errorf("a clean status carries an error key: %s", b)
	}
}

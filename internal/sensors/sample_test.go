package sensors

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// logSink keeps what was logged, so we can check a broken sensor is reported once and not every
// interval afterwards.
type logSink struct {
	mu   sync.Mutex
	recs []string
}

func (s *logSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *logSink) Handle(_ context.Context, rec slog.Record) error {
	s.mu.Lock()
	s.recs = append(s.recs, rec.Level.String()+" "+rec.Message)
	s.mu.Unlock()
	return nil
}

func (s *logSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *logSink) WithGroup(string) slog.Handler      { return s }

func (s *logSink) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.recs...)
}

func TestExecReadsScalesAndDropsUnknownFields(t *testing.T) {
	r, clk := testReg(t)
	mustApply(t, r, Source{
		ID:       "cpu",
		Kind:     Exec,
		Command:  `printf 'temperature=21.5\nhumidity: 40\nwindspeed=3\n'`,
		Interval: 30 * time.Second,
		Scale:    map[Field]float64{Humidity: 2},
	})

	rd, err := r.ReadNow(context.Background(), "cpu")
	if err != nil {
		t.Fatalf("ReadNow: %v", err)
	}
	if rd.Fields[Temperature] != 21.5 {
		t.Errorf("temperature = %v, want 21.5", rd.Fields[Temperature])
	}
	if rd.Fields[Humidity] != 80 {
		t.Errorf("humidity = %v, want 40 × 2", rd.Fields[Humidity])
	}
	if _, bad := rd.Fields["windspeed"]; bad {
		t.Error("a field we can't carry was stored")
	}
	if !rd.At.Equal(clk.Now()) {
		t.Errorf("reading time = %s, want now", rd.At)
	}
	if st := statusByID(r)["cpu"]; st.Reads != 1 || st.Errors != 0 || !st.Fresh {
		t.Errorf("status = %+v, want one clean, fresh read", st)
	}
}

func TestExecFailures(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{"non-zero exit", `echo "i2c write failed" >&2; exit 3`, "command failed"},
		{"the reason it gave", `echo "i2c write failed" >&2; exit 3`, "i2c write failed"},
		{"nothing we understand", `echo hello there`, "no readings we understand"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := testReg(t)
			mustApply(t, r, Source{ID: "cpu", Kind: Exec, Command: tc.command, Interval: 30 * time.Second})
			_, err := r.ReadNow(context.Background(), "cpu")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadNow error = %v, want one mentioning %q", err, tc.want)
			}
			st := statusByID(r)["cpu"]
			if st.Errors != 1 || st.Reads != 0 || !strings.Contains(st.Err, tc.want) {
				t.Errorf("status = %+v, want one failure the GUI can show", st)
			}
		})
	}
}

func TestExecTimeoutIsCappedAndRecorded(t *testing.T) {
	r, _ := testReg(t)
	r.maxWait = 100 * time.Millisecond // stands in for the 10s cap
	mustApply(t, r, Source{ID: "cpu", Kind: Exec, Command: `sleep 30`, Interval: time.Minute})

	start := time.Now()
	_, err := r.ReadNow(context.Background(), "cpu")
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a hung command held the read for %s; the cap didn't apply", took)
	}
	if err == nil || !strings.Contains(err.Error(), "still running after") {
		t.Fatalf("ReadNow error = %v, want one about the command being killed", err)
	}
	if st := statusByID(r)["cpu"]; st.Errors != 1 {
		t.Errorf("status = %+v, want the timeout counted", st)
	}
}

func TestExecWaitIsTheSmallerOfIntervalAndCap(t *testing.T) {
	r, _ := testReg(t)
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{time.Minute, execCap},
		{5 * time.Second, 5 * time.Second},
		{0, execCap}, // a push source has none; the cap still applies
	}
	for _, tc := range cases {
		if got := r.execWait(Source{Interval: tc.interval}); got != tc.want {
			t.Errorf("execWait(interval %s) = %s, want %s", tc.interval, got, tc.want)
		}
	}
}

func TestFileSourceAndMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values")
	if err := os.WriteFile(path, []byte("# written by the weather script\ntemperature=4.25\ndistance=1200\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := testReg(t)
	mustApply(t, r, Source{
		ID:       "weather",
		Kind:     File,
		Path:     path,
		Interval: 30 * time.Second,
		Scale:    map[Field]float64{Distance: 0.001},
	})

	rd, err := r.ReadNow(context.Background(), "weather")
	if err != nil {
		t.Fatalf("ReadNow: %v", err)
	}
	if rd.Fields[Temperature] != 4.25 || rd.Fields[Distance] != 1.2 {
		t.Fatalf("readings = %+v, want 4.25 °C and a scaled distance", rd.Fields)
	}

	// A file that isn't there is an error the GUI shows, not a crash, and the last reading stays.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err = r.ReadNow(context.Background(), "weather")
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "check the path") {
		t.Fatalf("ReadNow error = %v, want one naming the file and what to do", err)
	}
	if st := statusByID(r)["weather"]; st.Errors != 1 || st.Reads != 1 || st.Last.Fields[Temperature] != 4.25 {
		t.Errorf("status = %+v, want the old reading kept beside the error", st)
	}

	// A file with nothing we understand is an error too, not an empty reading.
	if err := os.WriteFile(path, []byte("all quiet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadNow(context.Background(), "weather"); err == nil || !strings.Contains(err.Error(), "no readings we understand") {
		t.Fatalf("ReadNow error = %v, want one about the contents", err)
	}
}

func TestFailureIsLoggedOnceUntilItReadsAgain(t *testing.T) {
	sink := &logSink{}
	path := filepath.Join(t.TempDir(), "values")
	r, _ := testReg(t)
	r.log = slog.New(sink)
	mustApply(t, r, Source{ID: "weather", Kind: File, Path: path, Interval: 30 * time.Second})

	ctx := context.Background()
	for i := 0; i < 3; i++ { // the file isn't there yet
		if _, err := r.ReadNow(ctx, "weather"); err == nil {
			t.Fatal("reading a missing file succeeded")
		}
	}
	if got := sink.lines(); len(got) != 1 || !strings.HasPrefix(got[0], "WARN") {
		t.Fatalf("log after three failures = %v, want one warning", got)
	}

	if err := os.WriteFile(path, []byte("temperature=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadNow(ctx, "weather"); err != nil {
		t.Fatalf("ReadNow: %v", err)
	}
	if got := sink.lines(); len(got) != 2 || !strings.HasPrefix(got[1], "INFO") {
		t.Fatalf("log after it recovered = %v, want a second line saying so", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadNow(ctx, "weather"); err == nil {
		t.Fatal("reading a missing file succeeded")
	}
	if got := sink.lines(); len(got) != 3 || !strings.HasPrefix(got[2], "WARN") {
		t.Fatalf("log after it broke again = %v, want a fresh warning", got)
	}
}

func TestReadNowUnknownAndPush(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r, Source{ID: "rain", Kind: Push})

	if _, err := r.ReadNow(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "no sensor called") {
		t.Fatalf("ReadNow of an unknown id = %v, want an error saying so", err)
	}
	if _, err := r.ReadNow(context.Background(), "rain"); err == nil || !strings.Contains(err.Error(), "pushed to") {
		t.Fatalf("ReadNow of an unread push sensor = %v, want an error explaining why", err)
	}
	if err := r.Push("rain", map[Field]float64{Rainfall1h: 3}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	rd, err := r.ReadNow(context.Background(), "rain")
	if err != nil || rd.Fields[Rainfall1h] != 3 {
		t.Fatalf("ReadNow of a pushed sensor = %+v, %v; want the last pushed reading", rd, err)
	}
}

func TestRunSamplesEditsAndStops(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r, Source{ID: "cpu", Kind: Exec, Command: `echo temperature=30`, Interval: 5 * time.Second})
	ids, stop := r.Subscribe(8)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	waitFor(t, ids, "cpu") // a loop reads straight away, it doesn't wait out the interval

	// A sensor added while we're running is picked up without a restart.
	if err := r.Add(Source{ID: "lux", Kind: Exec, Command: `echo lux=120`, Interval: 5 * time.Second}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFor(t, ids, "lux")
	if rd, ok := r.Latest("lux"); !ok || rd.Fields[Lux] != 120 {
		t.Errorf("Latest(lux) = %+v, %v", rd, ok)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return when its context ended")
	}
}

func TestRunIgnoresPushSources(t *testing.T) {
	r, _ := testReg(t)
	mustApply(t, r, Source{ID: "rain", Kind: Push})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if st := statusByID(r)["rain"]; st.Reads != 0 || st.Errors != 0 {
		t.Errorf("status = %+v, want a push source left alone by Run", st)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return when its context ended")
	}
}

func TestFingerprintDecidesWhatRestartsALoop(t *testing.T) {
	base := Source{ID: "cpu", Kind: Exec, Command: "read-temp", Interval: time.Minute, Scale: map[Field]float64{Temperature: 2}}
	cases := []struct {
		name string
		src  Source
		same bool
	}{
		{"same source", base, true},
		{"renamed only", func() Source { s := base.clone(); s.Name = "Cabinet"; return s }(), true},
		{"new command", func() Source { s := base.clone(); s.Command = "read-temp -v"; return s }(), false},
		{"new interval", func() Source { s := base.clone(); s.Interval = 30 * time.Second; return s }(), false},
		{"new scale", func() Source { s := base.clone(); s.Scale[Temperature] = 3; return s }(), false},
		{"other kind", func() Source { s := base.clone(); s.Kind = File; s.Path = "/tmp/x"; return s }(), false},
	}
	want := fingerprint(base)
	for _, tc := range cases {
		if got := fingerprint(tc.src) == want; got != tc.same {
			t.Errorf("%s: fingerprint unchanged = %v, want %v", tc.name, got, tc.same)
		}
	}
}

// waitFor reads ids until the wanted one turns up, or the test gives up.
func waitFor(t *testing.T, ids <-chan string, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-ids:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("no reading for %q", want)
		}
	}
}

// A command that never stops printing must cost us a fixed amount of memory, not the daemon: one
// wrong command in a config field used to be enough to have the repeater OOM-killed.
func TestExecOutputIsCapped(t *testing.T) {
	r, _ := testReg(t)
	r.maxWait = 2 * time.Second
	mustApply(t, r, Source{ID: "flood", Kind: Exec, Command: `yes temperature=21.5`, Interval: time.Minute})

	_, err := r.ReadNow(context.Background(), "flood")
	if err == nil || !strings.Contains(err.Error(), "printed more than") {
		t.Fatalf("ReadNow error = %v, want one about the command printing too much", err)
	}
}

// The writer keeps its limit and remembers that it threw output away.
func TestCappedWriter(t *testing.T) {
	c := &capped{limit: 8}
	n, err := c.Write([]byte("12345"))
	if n != 5 || err != nil || c.dropped {
		t.Fatalf("short write: n=%d err=%v dropped=%v", n, err, c.dropped)
	}
	// A writer that reported a short count would make the command fail with "broken pipe" instead
	// of finishing, so the whole write is always claimed.
	if n, err := c.Write([]byte("6789abc")); n != 7 || err != nil {
		t.Fatalf("overflowing write: n=%d err=%v", n, err)
	}
	if got := string(c.Bytes()); got != "12345678" || !c.dropped {
		t.Fatalf("kept %q dropped=%v, want the first 8 bytes and the flag set", got, c.dropped)
	}
}

// A timed-out command takes its children with it. Killing only the shell left them running, one per
// interval, for as long as the daemon ran.
func TestExecTimeoutKillsTheWholePipeline(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available")
	}
	marker := fmt.Sprintf("rt-sensor-orphan-%d", time.Now().UnixNano())
	r, _ := testReg(t)
	r.maxWait = 300 * time.Millisecond
	mustApply(t, r, Source{ID: "hang", Kind: Exec,
		Command: fmt.Sprintf(`sh -c 'sleep 45 %s' & wait`, marker), Interval: time.Minute})
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", marker).Run() })

	if _, err := r.ReadNow(context.Background(), "hang"); err == nil {
		t.Fatal("a hung command was not killed")
	}
	// Give the kernel a moment to reap the group before looking for survivors.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := exec.Command("pgrep", "-f", marker).Output(); len(bytes.TrimSpace(out)) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a child of the timed-out command was still running")
}

// Try runs a source that hasn't been saved, and leaves nothing behind.
func TestTryRunsAnUnsavedSource(t *testing.T) {
	r, _ := testReg(t)
	rd, err := r.Try(context.Background(), Source{ID: "draft", Kind: Exec, Command: "echo temperature=12.5", Interval: time.Minute})
	if err != nil || rd.Fields[Temperature] != 12.5 {
		t.Fatalf("Try = %+v, %v", rd, err)
	}
	if len(r.Sources()) != 0 || len(r.Status()) != 0 {
		t.Error("testing a sensor registered it")
	}
	if _, err := r.Try(context.Background(), Source{ID: "draft", Kind: Exec, Command: "exit 4", Interval: time.Minute}); err == nil {
		t.Error("a failing command tested clean")
	}
	if _, err := r.Try(context.Background(), Source{ID: "draft", Kind: Push}); err == nil ||
		!strings.Contains(err.Error(), "nothing to test") {
		t.Errorf("push source: %v", err)
	}
	if _, err := r.Try(context.Background(), Source{Kind: Exec, Command: "true"}); err == nil {
		t.Error("an invalid source was tested rather than refused")
	}
}

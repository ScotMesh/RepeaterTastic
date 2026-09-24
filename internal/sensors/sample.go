package sensors

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// execCap is the longest we ever wait for a command, however long the interval is. A sensor script
// that hangs (a wedged I²C bus, a lost network mount) must not stop the loop from reading again.
const execCap = 10 * time.Second

// execOutputCap is how much of a command's output we keep. A reading is a few dozen bytes; a
// command printing without end ("yes", a log file, a binary) would otherwise fill memory until the
// daemon is killed, taking the repeater off air over a typo in a config field.
const execOutputCap = 64 << 10

// Run samples every exec and file source until ctx ends. Push sources need nothing here: their
// readings arrive through Push.
func (r *Registry) Run(ctx context.Context) {
	loops := map[string]*loop{}
	defer func() {
		for _, l := range loops {
			l.stop()
		}
	}()
	for {
		r.syncLoops(ctx, loops)
		select {
		case <-ctx.Done():
			return
		case <-r.changed:
		}
	}
}

// loop is one running sampler. fp is the part of the source that decides how it is read, so an edit
// that changes the command, the path or the interval restarts it and a rename of the name alone
// doesn't.
type loop struct {
	fp     string
	cancel context.CancelFunc
	done   chan struct{}
}

// stop ends a sampler and waits for it, so a removed sensor can't record a reading afterwards.
func (l *loop) stop() {
	l.cancel()
	<-l.done
}

// syncLoops starts a sampler for every source that wants one and stops the rest.
func (r *Registry) syncLoops(ctx context.Context, loops map[string]*loop) {
	want := map[string]Source{}
	for _, s := range r.Sources() {
		if s.Kind == Push {
			continue
		}
		want[s.ID] = s
	}
	for id, l := range loops {
		if s, keep := want[id]; keep && l.fp == fingerprint(s) {
			continue
		}
		l.stop()
		delete(loops, id)
	}
	for id, s := range want {
		if _, running := loops[id]; running {
			continue
		}
		lctx, cancel := context.WithCancel(ctx)
		l := &loop{fp: fingerprint(s), cancel: cancel, done: make(chan struct{})}
		loops[id] = l
		go func(s Source) {
			defer close(l.done)
			r.sampleLoop(lctx, s)
		}(s)
	}
}

// sampleLoop reads one source until ctx ends. It reads once straight away, so a sensor just added in
// the GUI shows a value without waiting out its interval.
func (r *Registry) sampleLoop(ctx context.Context, s Source) {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		rd, err := r.sample(ctx, s)
		if ctx.Err() != nil {
			return // shutting down: a cancelled read isn't the sensor's fault, don't record it
		}
		r.record(s.ID, rd, err)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReadNow reads a source at once, for the GUI's "Read now". The result is recorded like any other
// read, so everyone sees the same reading, or the same error.
func (r *Registry) ReadNow(ctx context.Context, id string) (Reading, error) {
	r.mu.RLock()
	e, ok := r.entries[id]
	var src Source
	if ok {
		src = e.src
	}
	r.mu.RUnlock()

	if !ok {
		return Reading{}, fmt.Errorf("no sensor called %q", id)
	}
	if src.Kind == Push {
		if rd, have := r.Latest(id); have {
			return rd, nil
		}
		return Reading{}, fmt.Errorf("sensor %q is pushed to, so there is nothing here to read; it has had no readings yet", id)
	}
	rd, err := r.sample(ctx, src)
	r.record(id, rd, err)
	if err != nil {
		return Reading{}, err
	}
	return rd, nil
}

// sample takes one reading, whichever way this source produces them.
func (r *Registry) sample(ctx context.Context, s Source) (Reading, error) {
	switch s.Kind {
	case Exec:
		return r.readExec(ctx, s)
	case File:
		return r.readFile(s)
	}
	return Reading{}, fmt.Errorf("sensor %q is kind %s: there is nothing to sample", s.ID, s.Kind)
}

// readExec runs the command and parses what it prints.
func (r *Registry) readExec(ctx context.Context, s Source) (Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, r.execWait(s))
	defer cancel()
	// A shell, because a command is one line of config and is usually a pipeline or a redirect.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", s.Command)
	// Its own process group, so a timeout kills the whole pipeline. Killing the shell alone leaves
	// its children running, and a command that hangs every interval would pile them up for ever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	// If something in the pipeline still holds the output pipe open after the kill, give up on it
	// rather than let this read block for ever.
	cmd.WaitDelay = 2 * time.Second
	// Stop a flooding command as soon as it passes the cap: there is no reading coming, and it
	// would otherwise burn CPU until its timeout.
	out, stderr := &capped{limit: execOutputCap, full: cancel}, &capped{limit: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	if out.dropped {
		return Reading{}, fmt.Errorf("sensor %q: the command printed more than %d KB and was stopped; print one \"temperature=21.5\" per line and exit",
			s.ID, execOutputCap>>10)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Reading{}, fmt.Errorf("sensor %q: command still running after %s, so it was killed; make it print a reading and exit", s.ID, r.execWait(s))
		}
		return Reading{}, fmt.Errorf("sensor %q: command failed: %w%s", s.ID, err, detail(stderr.Bytes()))
	}
	vals := scaleFields(s.Scale, ParseValues(out.Bytes()))
	if len(vals) == 0 {
		return Reading{}, fmt.Errorf("sensor %q: the command printed no readings we understand; print one \"temperature=21.5\" per line%s", s.ID, detail(out.Bytes()))
	}
	return Reading{At: r.now(), Fields: vals}, nil
}

// readFile reads the file something else keeps up to date. A file that isn't there yet is an error
// state the GUI shows, not a reason to stop reading it.
func (r *Registry) readFile(s Source) (Reading, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Reading{}, fmt.Errorf("sensor %q: nothing at %s yet; check the path, and that whatever writes it is running", s.ID, s.Path)
		}
		return Reading{}, fmt.Errorf("sensor %q: cannot read %s: %w", s.ID, s.Path, err)
	}
	vals := scaleFields(s.Scale, ParseValues(b))
	if len(vals) == 0 {
		return Reading{}, fmt.Errorf("sensor %q: %s holds no readings we understand; it needs one \"temperature=21.5\" per line%s", s.ID, s.Path, detail(b))
	}
	return Reading{At: r.now(), Fields: vals}, nil
}

// killGroup kills the command's whole process group, falling back to the process itself if it never
// got a group of its own.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && pgid == cmd.Process.Pid {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			return nil
		}
	}
	return cmd.Process.Kill()
}

// capped collects output up to a limit and throws the rest away, so a command that never stops
// printing costs us a fixed amount of memory. It never fails the write: the command is left to
// finish or time out on its own.
type capped struct {
	limit int
	// full is called once, if set, the first time output is thrown away: the caller uses it to stop
	// the command.
	full    func()
	buf     bytes.Buffer
	dropped bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) <= room {
			return c.buf.Write(p)
		}
		_, _ = c.buf.Write(p[:room])
	}
	if !c.dropped {
		c.dropped = true
		if c.full != nil {
			c.full()
		}
	}
	return len(p), nil // never a short write: that would fail the command with a broken pipe
}

// Bytes is what was kept.
func (c *capped) Bytes() []byte { return c.buf.Bytes() }

// execWait is how long this source's command may take: its interval, or the cap, whichever is
// smaller, so a read can never overrun into the next one.
func (r *Registry) execWait(s Source) time.Duration {
	wait := r.maxWait
	if s.Interval > 0 && s.Interval < wait {
		wait = s.Interval
	}
	return wait
}

// fingerprint is everything about a source that changes how it is read.
func fingerprint(s Source) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s", s.Kind, s.Command, s.Path, s.Interval)
	for _, f := range Fields { // in Fields order: a map would come out differently each time
		if m, ok := s.Scale[f]; ok {
			fmt.Fprintf(&b, "|%s=%g", f, m)
		}
	}
	return b.String()
}

// detail quotes the first line of a command's output in an error, so the GUI shows why it failed
// without pasting a screenful.
func detail(b []byte) string {
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return ""
	}
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return ": " + line
}

// The I²C shim a hosted node preloads. The library is built from shim/ (`make shim`), carried
// inside the binary and extracted into each node's instance directory, so hosting a sensor needs
// nothing installed on the host.

package nodes

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// shimFS holds the built shim libraries, one per architecture: shim/i2cshim-linux-<goarch>.so, put
// there by `make shim`. The directory is committed with only a .gitkeep in it, so the pattern always
// matches and a build without the libraries still compiles: a node that needs one fails to start
// with an error that says what to run.
//
//go:embed shim/*
var shimFS embed.FS

// shimName is the library for an architecture, as `make shim` names it.
func shimName(goarch string) string { return "i2cshim-linux-" + goarch + ".so" }

// shimLib is the embedded library for the architecture we run on.
func shimLib() ([]byte, error) {
	name := shimName(runtime.GOARCH)
	b, err := shimFS.ReadFile("shim/" + name)
	if err != nil {
		return nil, fmt.Errorf("sensors need shim/build/%s: run make shim (this build carries no I²C shim for linux/%s)",
			name, runtime.GOARCH)
	}
	return b, nil
}

// shimLibrary is where extractShim gets the library. It is a variable so a test can stand in for a
// build that has one without carrying a real shared object in the tree.
var shimLibrary = shimLib

// HasShim reports whether this build carries the I²C shim for the machine it runs on, so the GUI
// and the daemon can say that sensors can't be hosted before anyone attaches one.
func HasShim() bool {
	_, err := shimLib()
	return err == nil
}

// extractShim puts the shim library in a node's instance directory, ready to be preloaded. It
// writes through a temporary file and renames, so a running node keeps the library it has mapped,
// and it leaves an identical file alone.
func extractShim(dir string) error {
	b, err := shimLibrary()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, shimFile)
	if old, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(old, b) {
		return nil
	}
	f, err := os.CreateTemp(dir, "."+shimFile+".tmp")
	if err != nil {
		return fmt.Errorf("cannot unpack the I²C shim into %s: %w", dir, err)
	}
	tmp := f.Name()
	if err := writeShim(f, b); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cannot unpack the I²C shim into %s: %w", dir, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cannot replace %s: %w", path, err)
	}
	return nil
}

// writeShim fills and closes the library file, executable as a shared object must be.
func writeShim(f *os.File, b []byte) error {
	write := func() error {
		if err := f.Chmod(0o755); err != nil {
			return err
		}
		_, err := f.Write(b)
		return err
	}
	err := write()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

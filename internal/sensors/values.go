package sensors

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// WriteValuesFile writes the readings file a node's shim reads, atomically, and reports whether the
// contents actually changed. Every source is written to every node that publishes it each time it
// is read, so without the comparison an unchanged reading would still rewrite (and re-timestamp)
// dozens of files a minute; callers use changed to decide whether anything needs doing.
func WriteValuesFile(path string, b []byte) (changed bool, err error) {
	if old, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(old, b) {
		return false, nil
	}
	dir := filepath.Dir(path)
	// Temp file in the same directory, so the rename is atomic: a reader sees the old file or the
	// new one, never half of either.
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return false, fmt.Errorf("cannot write %s: %w; check the directory exists and is writable", path, err)
	}
	tmp := f.Name()
	if err := fill(f, b); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("cannot replace %s: %w", path, err)
	}
	return true, nil
}

// fill writes the contents and closes the file, leaving it readable only by us: a readings file sits
// in a node's own directory and nothing else needs it.
func fill(f *os.File, b []byte) error {
	write := func() error {
		if err := f.Chmod(0o600); err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			return err
		}
		// Sync before the rename, so a power cut can't leave the shim a truncated file.
		return f.Sync()
	}
	err := write()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

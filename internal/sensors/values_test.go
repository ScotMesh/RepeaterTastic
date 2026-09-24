package sensors

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteValuesFileReportsRealChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "values")

	changed, err := WriteValuesFile(path, []byte("temperature=21.5000\n"))
	if err != nil || !changed {
		t.Fatalf("first write: changed = %v, err = %v; want true, nil", changed, err)
	}
	if changed, err = WriteValuesFile(path, []byte("temperature=21.5000\n")); err != nil || changed {
		t.Fatalf("rewrite of the same reading: changed = %v, err = %v; want false, nil", changed, err)
	}
	if changed, err = WriteValuesFile(path, []byte("temperature=21.6000\n")); err != nil || !changed {
		t.Fatalf("write of a new reading: changed = %v, err = %v; want true, nil", changed, err)
	}

	b, err := os.ReadFile(path)
	if err != nil || string(b) != "temperature=21.6000\n" {
		t.Fatalf("file holds %q, %v", b, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %v, want 0600", got)
	}

	// The rename must leave no temp files behind for the shim to trip over.
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Errorf("directory holds %d files, want just the readings file", len(names))
	}
}

func TestWriteValuesFileErrorNamesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "values")
	changed, err := WriteValuesFile(path, []byte("temperature=1.0000\n"))
	if err == nil || changed {
		t.Fatalf("write into a missing directory: changed = %v, err = %v; want false and an error", changed, err)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "writable") {
		t.Errorf("error = %v, want one naming the file and what to check", err)
	}
}

func TestRenderValuesParseValuesRoundTrip(t *testing.T) {
	fields := map[Field]float64{}
	for i, f := range Fields {
		fields[f] = float64(i) + 0.5
	}
	rd := Reading{Fields: fields}

	got := ParseValues(RenderValues(rd, nil))
	for f, want := range fields {
		if !Carried(f) {
			if _, in := got[f]; in {
				t.Errorf("%s came back although no chip carries it", f)
			}
			continue
		}
		if math.Abs(got[f]-want) > 1e-4 { // RenderValues writes four decimals
			t.Errorf("%s = %v, want %v", f, got[f], want)
		}
	}

	// A file written and read back the way the shim does it holds the same readings.
	path := filepath.Join(t.TempDir(), "values")
	if _, err := WriteValuesFile(path, RenderValues(rd, nil)); err != nil {
		t.Fatalf("WriteValuesFile: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if again := ParseValues(b); len(again) != len(got) {
		t.Errorf("read back %d fields, want %d", len(again), len(got))
	}
}

func TestRenderValuesNarrowsToWantedFields(t *testing.T) {
	rd := Reading{Fields: map[Field]float64{Temperature: 21.5, Humidity: 40, PM25: 12}}
	out := string(RenderValues(rd, []Field{Temperature, PM25}))
	if out != "pm25=12.0000\ntemperature=21.5000\n" {
		t.Errorf("RenderValues = %q, want pm25 and temperature, sorted", out)
	}
	if RenderValues(Reading{}, nil) != nil {
		t.Error("a reading with no fields wrote a file")
	}
	// Radiation is a field we know but no chip may carry, so it is left out of the file.
	if got := string(RenderValues(Reading{Fields: map[Field]float64{Radiation: 13.7, Temperature: 1}}, nil)); strings.Contains(got, "radiation") {
		t.Errorf("RenderValues = %q, want radiation left out", got)
	}
}

func TestScaleFieldsDropsValuesThatArentNumbers(t *testing.T) {
	got := scaleFields(map[Field]float64{Temperature: 2}, map[Field]float64{
		Temperature: 10,
		Humidity:    math.NaN(),
		Lux:         math.Inf(1),
		"windspeed": 4,
	})
	if len(got) != 1 || got[Temperature] != 20 {
		t.Errorf("scaleFields = %+v, want only a scaled temperature", got)
	}
}

func TestPlanChipsCarriesWhatWeRender(t *testing.T) {
	cases := []struct {
		name        string
		fields      []Field
		chips       string
		unsupported []Field
	}{
		{"one field, one chip", []Field{Temperature}, "pct2075", nil},
		{"two fields on one chip", []Field{Voltage, Current}, "ina226", nil},
		{"chips come in a fixed order", []Field{PM25, Humidity}, "aht10,pmsa003i", nil},
		{"pressure and temperature, two chips", []Field{Temperature, Pressure}, "pct2075,bmp280", nil},
		{"lux rides on a BH1750", []Field{Lux}, "bh1750", nil},
		{"radiation is the one field with no chip", []Field{Radiation}, "", []Field{Radiation}},
		{"nothing at all", nil, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chips, unsupported := PlanChips(tc.fields)
			if got := ChipNames(chips); got != tc.chips {
				t.Errorf("ChipNames = %q, want %q", got, tc.chips)
			}
			if len(unsupported) != len(tc.unsupported) {
				t.Fatalf("unsupported = %v, want %v", unsupported, tc.unsupported)
			}
			for i, f := range tc.unsupported {
				if unsupported[i] != f {
					t.Errorf("unsupported[%d] = %q, want %q", i, unsupported[i], f)
				}
			}
		})
	}
}

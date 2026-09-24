package sensors

import "testing"

// An id names a file in a node's directory and a path in the API, so only safe ones are allowed.
func TestSourceIDValidation(t *testing.T) {
	bad := []string{"", "a b", "a/b", "a\\b", "a\tb", "..", "../etc", "a..b", ".hidden", "-lead", "_lead"}
	for _, id := range bad {
		s := Source{ID: id, Kind: Push}
		if err := s.Validate(); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
	good := []string{"shed", "Shed-2", "rack_psu", "bme280.outside", "a"}
	for _, id := range good {
		s := Source{ID: id, Kind: Push}
		if err := s.Validate(); err != nil {
			t.Errorf("id %q: %v", id, err)
		}
	}
}

// Publishable and ChipFor must agree with the chips the shim actually implements.
func TestPublishableMatchesTheChips(t *testing.T) {
	want := []Field{Temperature, Humidity, Pressure, Lux, Voltage, Current, PM10, PM25, PM100,
		Distance, Rainfall1h, Rainfall24h}
	got := Publishable()
	if len(got) != len(want) {
		t.Fatalf("publishable = %v, want %v", got, want)
	}
	for _, f := range want {
		if !Carried(f) || ChipFor(f) == "" {
			t.Errorf("%s is not carried", f)
		}
	}
	// Radiation is the one field we can plan a chip for but must not: see Chips.
	for _, f := range []Field{Radiation} {
		if Carried(f) || ChipFor(f) != "" {
			t.Errorf("%s is claimed as carried but no chip is implemented", f)
		}
	}
}

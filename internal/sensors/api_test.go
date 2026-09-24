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

// A barometer and a humidity sensor both measure temperature, and the firmware publishes what they
// report — so planning either without a temperature reading would put an invented one on the mesh.
// Proven on a real node: pressure alone broadcast {'temperature': 20.0, pressure...}.
func TestInventsNamesFieldsNobodyAttached(t *testing.T) {
	cases := []struct {
		name   string
		fields []Field
		want   []Field
	}{
		{"pressure alone brings a temperature with it", []Field{Pressure}, []Field{Temperature}},
		{"so does humidity", []Field{Humidity}, []Field{Temperature}},
		{"with temperature attached, nothing is invented", []Field{Pressure, Temperature}, nil},
		{"and it is named once, not per chip", []Field{Pressure, Humidity}, []Field{Temperature}},
		{"a thermometer invents nothing", []Field{Temperature}, nil},
		{"nor does anything else", []Field{Lux, Distance, PM25, Voltage, Rainfall1h}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Invents(tc.fields)
			if len(got) != len(tc.want) {
				t.Fatalf("Invents(%v) = %v, want %v", tc.fields, got, tc.want)
			}
			for i, f := range tc.want {
				if got[i] != f {
					t.Errorf("Invents(%v)[%d] = %s, want %s", tc.fields, i, got[i], f)
				}
			}
		})
	}
	if c := Carriers(Temperature); len(c) != 3 {
		t.Errorf("Carriers(temperature) = %v, want the three chips that report it", c)
	}
}

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

package mixer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveFrame is the captured live frame the bus names in this package were read
// from. It is owned by another package; these tests skip rather than fail when
// it is absent so that a move on that side cannot break this one.
const liveFrame = "../m2lx/testdata/switcher_status-live-2026-07-31.json"

// TestBusWireValues pins every Bus constant to its exact wire string.
//
// This is not a tautology test. A Bus value is used directly as a map key
// against a parsed frame and directly inside a set_routing argument, so a
// mistyped constant does not fail loudly — it silently addresses a bus that
// does not exist, and a routing "fix" aimed at the clean feed would be
// accepted and do nothing. A typo in exactly this table was caught by hand
// while writing this package.
func TestBusWireValues(t *testing.T) {
	tests := []struct {
		name string
		bus  Bus
		want string
	}{
		{"master", BusMaster, "master"},
		{"aux1 is the clean feed", BusAux1, "aux1"},
		{"aux2 has no egress", BusAux2, "aux2"},
		{"mon1", BusMon1, "mon1"},
		{"mon2", BusMon2, "mon2"},
		{"mon3", BusMon3, "mon3"},
		{"mon4 is PFL", BusMon4, "mon4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.bus) != tt.want {
				t.Errorf("wire value = %q, want %q", string(tt.bus), tt.want)
			}
		})
	}
}

// TestAllBusesIsCompleteAndOrdered checks that AllBuses carries all seven
// buses exactly once, with the two buses that have egress first. Renderers and
// Compare iterate AllBuses to get stable output, so a missing entry would
// silently hide a bus from the drawer — including aux1, the clean feed.
func TestAllBusesIsCompleteAndOrdered(t *testing.T) {
	if len(AllBuses) != 7 {
		t.Fatalf("len(AllBuses) = %d, want 7", len(AllBuses))
	}
	if AllBuses[0] != BusMaster || AllBuses[1] != BusAux1 {
		t.Errorf("AllBuses starts %v, want the buses with egress first (master, aux1)", AllBuses[:2])
	}
	seen := map[Bus]int{}
	for _, b := range AllBuses {
		seen[b]++
	}
	for _, b := range []Bus{BusMaster, BusAux1, BusAux2, BusMon1, BusMon2, BusMon3, BusMon4} {
		if seen[b] != 1 {
			t.Errorf("bus %q appears %d times in AllBuses, want exactly 1", b, seen[b])
		}
	}
}

// TestBusLabel covers the one function in this package with a real body.
//
// The labels are the mitigation for an operator putting commentary into the
// clean feed without realising, so the assertions are about the specific words
// that carry that warning, not merely that a string comes back.
func TestBusLabel(t *testing.T) {
	tests := []struct {
		name string
		bus  Bus
		want string
	}{
		{"master names PGM", BusMaster, "master (PGM)"},
		{"aux1 names CLN and says clean feed", BusAux1, "aux1 (CLN - clean feed)"},
		{"aux2 warns of no egress", BusAux2, "aux2 (no egress)"},
		{"mon1", BusMon1, "mon1"},
		{"mon2", BusMon2, "mon2"},
		{"mon3", BusMon3, "mon3"},
		{"mon4 names PFL", BusMon4, "mon4 (PFL)"},
		{"unknown bus is reportable, not blank", Bus("aux9"), "aux9 (unknown bus)"},
		{"empty bus is named, not blank", Bus(""), "(unnamed bus)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BusLabel(tt.bus); got != tt.want {
				t.Errorf("BusLabel(%q) = %q, want %q", string(tt.bus), got, tt.want)
			}
		})
	}
}

// TestBusLabelsAreDistinctAndNonEmpty guards the property the drawer depends
// on: two buses must never present to an operator as the same name, and no bus
// may present as nothing. aux1 and aux2 differ by one character in their wire
// names and mean entirely different things.
func TestBusLabelsAreDistinctAndNonEmpty(t *testing.T) {
	seen := map[string]Bus{}
	for _, b := range AllBuses {
		label := BusLabel(b)
		if strings.TrimSpace(label) == "" {
			t.Errorf("BusLabel(%q) is blank", b)
		}
		if prev, dup := seen[label]; dup {
			t.Errorf("BusLabel(%q) and BusLabel(%q) both return %q", prev, b, label)
		}
		seen[label] = b
	}
}

// TestBusConstantsMatchLiveFrame cross-checks every Bus constant against the
// bus names in a real switcher_status frame, so that the constants are pinned
// to the device rather than to this package's own opinion.
func TestBusConstantsMatchLiveFrame(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(liveFrame))
	if err != nil {
		t.Skipf("live frame not available (%v); wire values still covered by TestBusWireValues", err)
	}
	var frame struct {
		Status []struct {
			Node  string `json:"node"`
			State struct {
				Outputs map[string]json.RawMessage `json:"outputs"`
			} `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("unmarshal live frame: %v", err)
	}
	var outputs map[string]json.RawMessage
	for _, e := range frame.Status {
		if e.Node == "advanced_audio_mixer" {
			outputs = e.State.Outputs
			break
		}
	}
	if outputs == nil {
		t.Fatal("no advanced_audio_mixer node in the live frame")
	}
	if len(outputs) != len(AllBuses) {
		t.Errorf("live frame has %d buses, AllBuses has %d", len(outputs), len(AllBuses))
	}
	for _, b := range AllBuses {
		if _, ok := outputs[string(b)]; !ok {
			t.Errorf("bus %q (%s) is not present in the live frame's outputs", b, BusLabel(b))
		}
	}
}

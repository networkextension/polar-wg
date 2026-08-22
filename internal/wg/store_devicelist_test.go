package wg

import (
	"errors"
	"testing"
)

// countingScanner records how many destinations scanWGDevice asks for.
type countingScanner struct{ n int }

func (s *countingScanner) Scan(dest ...any) error { s.n = len(dest); return errors.New("count only") }

// TestListWGDevicesSelectMatchesScanner guards against the #46 regression:
// a column added to scanWGDevice but not to the hand-written list query
// made GET /api/admin/wg-devices 500 with "expected 23 destination
// arguments in Scan, not 24".
func TestListWGDevicesSelectMatchesScanner(t *testing.T) {
	cs := &countingScanner{}
	_, _ = scanWGDevice(cs, true)
	want := len(splitCols(wgDeviceColumns)) + 2 // + site slug, hub slug
	if cs.n != want {
		t.Fatalf("scanWGDevice(hasSlug) scans %d dests, wgDeviceColumns+slugs = %d", cs.n, want)
	}
	got := len(splitCols(wgDeviceListSelect))
	if got != cs.n {
		t.Fatalf("listWGDevices selects %d columns but scanWGDevice scans %d", got, cs.n)
	}
}

func splitCols(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

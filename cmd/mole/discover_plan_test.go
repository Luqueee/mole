package main

import (
	"reflect"
	"testing"

	"github.com/Luqueee/mole/internal/config"
)

func TestPlanListeners(t *testing.T) {
	found := []int{22, 80, 3000, 5432, 51234, 60001}
	exclude := map[int]bool{22: true}

	t.Run("no discover_range forwards every listener", func(t *testing.T) {
		all := config.PortRange{}
		forward, keep, skipped := planListeners(found, exclude, all.Contains)
		if want := []int{80, 3000, 5432, 51234, 60001}; !reflect.DeepEqual(forward, want) {
			t.Errorf("forward = %v, want %v", forward, want)
		}
		if len(skipped) != 0 {
			t.Errorf("skipped = %v, want none", skipped)
		}
		if keep[22] || !keep[60001] {
			t.Errorf("keep = %v: excluded port kept or listener dropped", keep)
		}
	})

	t.Run("discover_range limits new forwards, named ports pass", func(t *testing.T) {
		r := config.PortRange{Lo: 1024, Hi: 49151}
		named := map[int]bool{60001: true} // e.g. `mole ports add 60001`
		forward, keep, skipped := planListeners(found, exclude, func(p int) bool {
			return r.Contains(p) || named[p]
		})
		if want := []int{3000, 5432, 60001}; !reflect.DeepEqual(forward, want) {
			t.Errorf("forward = %v, want %v", forward, want)
		}
		if want := []int{80, 51234}; !reflect.DeepEqual(skipped, want) {
			t.Errorf("skipped = %v, want %v", skipped, want)
		}
		// Out-of-range listeners are still kept, so one already forwarded
		// at the user's request is not pruned while it listens.
		for _, p := range []int{80, 3000, 5432, 51234, 60001} {
			if !keep[p] {
				t.Errorf("keep missing listening port %d: %v", p, keep)
			}
		}
		if keep[22] {
			t.Error("excluded port 22 kept")
		}
	})
}

// TestPlanListeners_KeepsLiveOutOfRangeForward runs the plan through
// forwarder.retain: a forward outside discover_range (added live) survives
// while its remote listener exists and is pruned once it is gone.
func TestPlanListeners_KeepsLiveOutOfRangeForward(t *testing.T) {
	f := newRetainForwarder()
	port, _ := openLoopback(t, f)
	outside := func(int) bool { return false } // a range that excludes port

	forward, keep, _ := planListeners([]int{port}, nil, outside)
	if len(forward) != 0 {
		t.Fatalf("forward = %v, want nothing new", forward)
	}
	f.retain(keep)
	if !contains(f.ports(), port) {
		t.Fatalf("live out-of-range forward %d was pruned", port)
	}

	_, keep, _ = planListeners(nil, nil, outside)
	f.retain(keep)
	if contains(f.ports(), port) {
		t.Fatalf("forward %d survived after its remote listener went away", port)
	}
}

package admission

import "testing"

func TestGovernor_EnforcesTotalCapacity(t *testing.T) {
	t.Parallel()

	governor := New()
	for range MaxTotal {
		if !governor.TryAdmit(Parent, 0) {
			t.Fatal("expected parent capacity")
		}
	}

	if governor.TryAdmit(Parent, 0) {
		t.Fatal("admitted beyond total capacity")
	}

	governor.Release(Parent, 0)
	if !governor.TryAdmit(Parent, 0) {
		t.Fatal("released capacity was not reusable")
	}
}

func TestGovernor_EnforcesChildQuotas(t *testing.T) {
	t.Parallel()

	governor := New()
	const parentID = int64(42)

	for range MaxPerParent {
		if !governor.TryAdmit(Child, parentID) {
			t.Fatal("expected per-parent child capacity")
		}
	}

	if governor.TryAdmit(Child, parentID) {
		t.Fatal("admitted beyond per-parent child capacity")
	}

	for i := MaxPerParent; i < MaxChildren; i++ {
		if !governor.TryAdmit(Child, int64(i+100)) {
			t.Fatal("expected global child capacity")
		}
	}

	if governor.TryAdmit(Child, 999) {
		t.Fatal("admitted beyond global child capacity")
	}

	if governor.LiveChildren() != MaxChildren || governor.LiveTotal() != MaxChildren {
		t.Fatalf("unexpected gauges: children=%d total=%d", governor.LiveChildren(), governor.LiveTotal())
	}

	governor.Release(Child, parentID)
	if !governor.TryAdmit(Child, parentID) {
		t.Fatal("released per-parent capacity was not reusable")
	}
}

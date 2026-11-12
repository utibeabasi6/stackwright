package builders

import "testing"

func TestReserveWithNeedsTheFeature(t *testing.T) {
	old := &builder{id: "old", slots: 2, jobs: map[string]bool{}}
	current := &builder{id: "current", slots: 2, jobs: map[string]bool{}, features: []string{Toolbox}}
	h := &Hub{builders: map[string]*builder{"old": old}}
	if _, ok := h.ReserveWith("org", nil, "", Toolbox); ok {
		t.Error("a builder without the toolbox got an agent sandbox")
	}
	if s, ok := h.Reserve("org", nil, ""); !ok || s.ID != "old" {
		t.Errorf("a job on the old builder: %v %v", s, ok)
	}
	h.builders["current"] = current
	if s, ok := h.ReserveWith("org", nil, "", Toolbox); !ok || s.ID != "current" {
		t.Errorf("an agent sandbox: %v %v", s, ok)
	}
}

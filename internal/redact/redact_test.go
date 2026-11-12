package redact

import "testing"

func TestRedact(t *testing.T) {
	r := New(`hunter2"quoted`, "", "plainsecret")
	got := r.String(`{"value":"hunter2\"quoted","other":"plainsecret plainsecret","safe":"ok"}`)
	want := `{"value":"(sensitive value)","other":"(sensitive value) (sensitive value)","safe":"ok"}`
	if got != want {
		t.Errorf("got %s", got)
	}
	amp := New(`a&b"c`)
	for _, in := range []string{`{"v":"a&b\"c"}`, `{"v":"a&b\"c"}`} {
		if got := amp.String(in); got != `{"v":"(sensitive value)"}` {
			t.Errorf("%s: got %s", in, got)
		}
	}
	if New().String("unchanged") != "unchanged" {
		t.Error("no secrets changed the text")
	}
}

func TestASecretInsideAnotherMasksTheWhole(t *testing.T) {
	r := New("hunter22")
	r.Add("postgres://admin:hunter22@db.internal:5432/app")
	if got := r.String("dsn=postgres://admin:hunter22@db.internal:5432/app"); got != "dsn="+Mask {
		t.Errorf("got %q", got)
	}
}

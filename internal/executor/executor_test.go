package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

func TestUntarStaysInsideItsFolder(t *testing.T) {
	for _, name := range []string{"../escape.tf", "a/../../escape.tf"} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
		_ = tw.Close()
		_ = gz.Close()
		if err := untar(buf.Bytes(), t.TempDir()+"/src"); err == nil {
			t.Errorf("%s: unpacked outside the folder", name)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		line, typ, addr, absent string
	}{
		{`{"type":"planned_change","change":{"resource":{"addr":"terraform_data.a"},"action":"create"}}`, "engine.planned_change", "terraform_data.a", ""},
		{`{"type":"apply_start","hook":{"resource":{"addr":"terraform_data.b"}}}`, "engine.apply_start", "terraform_data.b", ""},
		{`{"type":"diagnostic","diagnostic":{"severity":"error","address":"terraform_data.c"}}`, "engine.diagnostic", "terraform_data.c", ""},
		{`{"type":"outputs","outputs":{"s":{"sensitive":true,"value":"hunter2"},"p":{"sensitive":false,"value":"ok"}}}`, "engine.outputs", "", "hunter2"},
		{`Initializing the backend...`, "engine.log", "", ""},
	}
	for _, c := range cases {
		e := parse([]byte(c.line))
		if e.typ != c.typ || e.resourceAddr != c.addr {
			t.Errorf("%s: got %s %q", c.line, e.typ, e.resourceAddr)
		}
		if c.absent != "" && bytes.Contains(e.payload, []byte(c.absent)) {
			t.Errorf("%s: payload keeps %q: %s", c.line, c.absent, e.payload)
		}
	}
	if e := parse([]byte(`{"type":"outputs","outputs":{"p":{"value":"ok"}}}`)); !bytes.Contains(e.payload, []byte(`"ok"`)) {
		t.Errorf("a plain output lost its value: %s", e.payload)
	}
}

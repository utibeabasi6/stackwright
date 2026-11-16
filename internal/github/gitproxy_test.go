package github

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestCheckPushRefusesOtherRefs(t *testing.T) {
	const old, next, zero = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222",
		"0000000000000000000000000000000000000000"
	ref := "refs/heads/agent/fix-1234abcd"
	pkt := func(lines ...string) []byte {
		var b bytes.Buffer
		for _, l := range lines {
			fmt.Fprintf(&b, "%04x%s", len(l)+4, l)
		}
		b.WriteString("0000")
		return b.Bytes()
	}
	for name, c := range map[string]struct {
		raw []byte
		ok  bool
	}{
		"the session branch":           {pkt(old + " " + next + " " + ref + "\x00report-status\n"), true},
		"another branch":               {pkt(old + " " + next + " refs/heads/main\n"), false},
		"a second line with a tag":     {pkt(old+" "+next+" "+ref+"\n", zero+" "+next+" refs/tags/v1 x\n"), false},
		"the branch and a hidden ref":  {pkt(old + " " + next + " " + ref + " \n"), false},
		"a deletion":                   {pkt(old + " " + zero + " " + ref + "\n"), false},
		"a shallow line, then the ref": {pkt("shallow "+old+"\n", old+" "+next+" "+ref+"\n"), true},
	} {
		err := checkPush(bytes.NewReader(c.raw), "", ref)
		if c.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.ok && !errors.Is(err, ErrPushRefused) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
}

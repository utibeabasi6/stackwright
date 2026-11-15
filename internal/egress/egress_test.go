package egress

import (
	"net/netip"
	"testing"
)

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.215.14": true, "2606:2800:21f:cb07:6820:80da:af6b:8b2c": true,
		"10.1.2.3": false, "172.17.0.1": false, "192.168.65.254": false, "127.0.0.1": false, "169.254.169.254": false,
		"100.100.1.1": false, "0.250.250.254": false, "::ffff:10.0.0.1": false, "::ffff:0.250.250.254": false, "::1": false,
		"fe80::1": false, "fd00:ec2::254": false, "64:ff9b::a00:1": false, "224.0.0.1": false, "255.255.255.255": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %t", addr, got)
		}
	}
}

package netguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestPublicIP(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true, "104.16.0.1": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fec0::1": false, "100::1": false, "2001:2::1": false, "fc00::1": false, "::ffff:127.0.0.1": false,
		"::ffff:10.0.0.1": false, "64:ff9b::a00:1": false, "224.0.0.1": false, "255.255.255.255": false,
	} {
		if got := PublicIP(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicIP(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://api.openai.com/v1": true, "https://ollama.com": true, "https://8.8.8.8:8443/v1": true,
		"http://api.openai.com/v1": false, "https://localhost:1234/v1": false, "https://127.0.0.1/v1": false,
		"https://[::1]/v1": false, "https://gpu/v1": false, "https://box.local/v1": false, "https://x.internal/v1": false,
		"https://user:pw@api.openai.com/v1": false, "ftp://api.openai.com": false,
	} {
		if err := CheckURL(raw); (err == nil) != ok {
			t.Errorf("CheckURL(%s) = %v, want ok=%v", raw, err, ok)
		}
	}
}

// TestDialerRefusesLoopback: what a host name resolves to is checked when
// connecting, so a public-looking name pointing inside is refused too.
func TestDialerRefusesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = Dialer()(context.Background(), "tcp", ln.Addr().String())
	if !errors.Is(err, ErrNotPublic) {
		t.Fatalf("dial %s = %v, want ErrNotPublic", ln.Addr(), err)
	}
}

package xray

import "testing"

// A config with the "Russia past the VPN" domains must build: it needs the router and freedom.
func TestStartWithDirect(t *testing.T) {
	l, err := ParseLink("ss://YWVzLTI1Ni1nY206cGFzcw@1.2.3.4:8388#test")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := Start(l, "1.2.3.4", "ru", "xn--p1ai", "vk.com")
	if err != nil {
		t.Fatal(err)
	}
	inst.Close()
}

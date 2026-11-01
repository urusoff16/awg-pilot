package routing

import (
	"context"
	"errors"
	"math/rand"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func join(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, " ")
}

// covers reports whether addr is routed by the set.
func covers(ps []netip.Prefix, addr string) bool {
	a := netip.MustParseAddr(addr)
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func TestSubtractSingleHost(t *testing.T) {
	got := Subtract(mustPrefixes("0.0.0.0/0"), mustPrefixes("8.8.8.8/32"))
	if len(got) != 32 {
		t.Fatalf("expected 32 prefixes, got %d: %s", len(got), join(got))
	}
	if covers(got, "8.8.8.8") || !covers(got, "8.8.8.9") || !covers(got, "1.1.1.1") || !covers(got, "255.255.255.255") {
		t.Fatalf("wrong coverage: %s", join(got))
	}
}

func TestMergeJoinsSiblingsAndDropsCovered(t *testing.T) {
	got := Merge(mustPrefixes("10.0.0.0/9", "10.128.0.0/9", "10.1.2.0/24", "0.0.0.0/1", "128.0.0.0/1"))
	if join(got) != "0.0.0.0/0" {
		t.Fatalf("got %s", join(got))
	}
}

func randPrefix(r *rand.Rand, minBits, maxBits int) netip.Prefix {
	var b [4]byte
	r.Read(b[:])
	return netip.PrefixFrom(netip.AddrFrom4(b), minBits+r.Intn(maxBits-minBits+1)).Masked()
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// TestSubtractMatchesBruteForce checks the range algorithm against direct membership tests.
func TestSubtractMatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for round := 0; round < 30; round++ {
		base := []netip.Prefix{randPrefix(r, 0, 12), randPrefix(r, 4, 16)}
		var excl []netip.Prefix
		for i := 0; i < 40; i++ {
			excl = append(excl, randPrefix(r, 6, 28))
		}
		got := Subtract(base, excl)
		for i := 0; i < 3000; i++ {
			var b [4]byte
			r.Read(b[:])
			a := netip.AddrFrom4(b)
			if i%3 == 0 { // bias samples towards prefix edges
				p := excl[r.Intn(len(excl))]
				a = p.Addr()
				if i%2 == 0 {
					s := spanOf(p)
					hi := s.hi
					a = netip.AddrFrom4([4]byte{byte(hi >> 24), byte(hi >> 16), byte(hi >> 8), byte(hi)})
				}
			}
			want := inAny(base, a) && !inAny(excl, a)
			if inAny(got, a) != want {
				t.Fatalf("round %d: %s routed=%v want %v", round, a, !want, want)
			}
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Overlaps(got[i]) {
				t.Fatalf("overlapping output %s %s", got[i-1], got[i])
			}
		}
	}
}

func TestMergeIsMinimal(t *testing.T) {
	got := Merge(mustPrefixes("10.0.0.0/25", "10.0.0.128/25", "10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24"))
	if join(got) != "10.0.0.0/22" {
		t.Fatalf("got %s", join(got))
	}
	if join(Merge(mustPrefixes("255.255.255.255/32", "255.255.255.254/32"))) != "255.255.255.254/31" {
		t.Fatal("top of address space mishandled")
	}
}

func TestSubtractManyExclusionsIsFast(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	var excl []netip.Prefix
	for i := 0; i < 12000; i++ {
		excl = append(excl, randPrefix(r, 16, 24))
	}
	start := time.Now()
	got := Subtract(mustPrefixes("0.0.0.0/0"), excl)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("too slow: %v for %d routes", d, len(got))
	}
	t.Logf("12000 exclusions → %d routes in %v", len(got), time.Since(start))
}

func TestComputeBypass(t *testing.T) {
	ru := mustPrefixes("5.0.0.0/8", "77.88.0.0/18")
	res, err := Compute(context.Background(), Settings{Mode: ModeFull}, nil, nil, ru)
	if err != nil {
		t.Fatal(err)
	}
	if res.FullTunnel || covers(res.AllowedIPs, "77.88.8.8") || covers(res.AllowedIPs, "5.1.2.3") || !covers(res.AllowedIPs, "8.8.8.8") {
		t.Fatalf("bypass wrong: full=%v", res.FullTunnel)
	}
	in, _ := Compute(context.Background(), Settings{Mode: ModeInclude, Entries: []string{"5.1.0.0/16"}}, nil, nil, ru)
	if !covers(in.AllowedIPs, "5.1.2.3") {
		t.Fatal("include mode must keep explicitly listed addresses even inside the bypass list")
	}
}

func TestComputeModes(t *testing.T) {
	r := fakeResolver{"example.com": {netip.MustParseAddr("93.184.216.34")}}
	dns := []netip.Addr{netip.MustParseAddr("172.27.0.1")}
	ctx := context.Background()

	full, err := Compute(ctx, Settings{Mode: ModeFull}, dns, r, nil)
	if err != nil || !full.FullTunnel || join(full.AllowedIPs) != "0.0.0.0/0" {
		t.Fatalf("full: %v %+v", err, full)
	}

	lan, _ := Compute(ctx, Settings{Mode: ModeFull, BypassLAN: true}, dns, r, nil)
	if lan.FullTunnel || covers(lan.AllowedIPs, "192.168.1.10") || !covers(lan.AllowedIPs, "1.1.1.1") {
		t.Fatalf("bypass lan wrong: %s", join(lan.AllowedIPs))
	}
	// the provider's DNS (172.27.0.1) sits inside 172.16.0.0/12 but must stay in the tunnel
	if !covers(lan.AllowedIPs, "172.27.0.1") || covers(lan.AllowedIPs, "172.27.0.2") {
		t.Fatal("tunnel DNS must stay routed through the tunnel when LAN is bypassed")
	}

	ex, _ := Compute(ctx, Settings{Mode: ModeExclude, Entries: []string{"example.com", "203.0.113.0/24", "bad entry!", "2001:db8::1"}}, dns, r, nil)
	if covers(ex.AllowedIPs, "93.184.216.34") || covers(ex.AllowedIPs, "203.0.113.7") || !covers(ex.AllowedIPs, "8.8.8.8") {
		t.Fatalf("exclude wrong: %s", join(ex.AllowedIPs))
	}
	if len(ex.Warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %v", ex.Warnings)
	}

	in, _ := Compute(ctx, Settings{Mode: ModeInclude, Entries: []string{"example.com"}}, dns, r, nil)
	if join(in.AllowedIPs) != "93.184.216.34/32 172.27.0.1/32" && join(in.AllowedIPs) != "172.27.0.1/32 93.184.216.34/32" {
		t.Fatalf("include wrong: %s", join(in.AllowedIPs))
	}
	if _, err := Compute(ctx, Settings{Mode: ModeInclude}, dns, r, nil); err == nil {
		t.Fatal("empty include accepted")
	}
}

package main

import (
	"net/netip"
	"sort"

	"awgpilot/internal/routing"
)

// routeBudget caps the routes handed to VpnService.Builder. Every route travels to the system in
// one binder transaction (about 1 MB for the whole process), so the full list of Russian
// networks (tens of thousands of entries) would make establish() fail.
const routeBudget = 2000

var allIPv4 = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}

// fitBypass keeps the largest blocks of bypass that fit the budget together with the fixed
// exclusions. Dropped (smaller) blocks simply go through the VPN, so nothing leaks. withExclude
// counts routes for Builder.excludeRoute (Android 13+); otherwise the complement is counted,
// because older systems need every included route listed. It also returns the share of bypass
// addresses that stay direct. keep (the tunnel DNS) is carved out of the exclusions, which can
// split a block into several routes, so it is counted too.
func fitBypass(fixed, bypass, keep []netip.Prefix, withExclude bool) ([]netip.Prefix, float64) {
	blocks := routing.Merge(bypass)
	if len(blocks) == 0 {
		return nil, 1
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].Bits() < blocks[j].Bits() })
	count := func(k int) int {
		ex := routing.Subtract(append(append([]netip.Prefix{}, fixed...), blocks[:k]...), keep)
		if withExclude {
			return len(ex)
		}
		return len(routing.Merge(append(routing.Subtract(allIPv4, ex), keep...)))
	}
	k := len(blocks)
	if count(k) > routeBudget {
		lo, hi := 0, len(blocks) // the answer is the largest k with count(k) <= budget
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if count(mid) <= routeBudget {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		k = lo
	}
	var total, kept float64
	for i, b := range blocks {
		n := float64(uint64(1) << (32 - b.Bits()))
		total += n
		if i < k {
			kept += n
		}
	}
	return blocks[:k], kept / total
}

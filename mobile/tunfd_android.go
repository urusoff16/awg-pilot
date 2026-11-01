package main

import "github.com/amnezia-vpn/amneziawg-go/v3/tun"

func tunFromFD(fd int) (tun.Device, error) {
	d, _, err := tun.CreateUnmonitoredTUNFromFD(fd)
	return d, err
}

//go:build !android

package main

import (
	"errors"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// tunFromFD exists only on Android; other builds compile the core for tests.
func tunFromFD(int) (tun.Device, error) { return nil, errors.New("только для Android") }

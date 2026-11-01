package store

import (
	"github.com/amnezia-vpn/amneziawg-windows/conf/dpapi"
)

const dpapiName = "AWGPilot secrets"

func protect(b []byte) ([]byte, error)   { return dpapi.Encrypt(b, dpapiName) }
func unprotect(b []byte) ([]byte, error) { return dpapi.Decrypt(b, dpapiName) }

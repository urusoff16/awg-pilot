package main

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

func TestEchoRequestAndReply(t *testing.T) {
	src, dst := netip.MustParseAddr("10.8.1.23"), netip.MustParseAddr("10.8.1.1")
	req := echoRequest(src, dst, 0x1235, 7)
	if checksum(req[:20]) != 0 || checksum(req[20:]) != 0 {
		t.Fatal("bad checksums")
	}
	reply := append([]byte(nil), req...)
	copy(reply[12:16], req[16:20])
	copy(reply[16:20], req[12:16])
	reply[20] = 0 // echo reply
	p := &pingTun{id: 0x1235, pending: map[uint16]time.Time{7: time.Now()}}
	p.rtt.Store(-1)
	if !p.isOurReply(reply) {
		t.Fatal("reply not recognised")
	}
	if p.rtt.Load() < 0 {
		t.Fatal("rtt not recorded")
	}
	binary.BigEndian.PutUint16(reply[24:], 0x9999)
	if p.isOurReply(reply) {
		t.Fatal("foreign echo reply swallowed")
	}
}

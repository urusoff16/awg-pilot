package service

import (
	"fmt"
	"testing"
)

func TestLogRingSequence(t *testing.T) {
	b := &backend{}
	for i := 0; i < 520; i++ {
		b.logf("line %d", i)
	}
	lines, next := b.Logs(0) // older than the ring: start from the oldest kept line
	if next != 520 || len(lines) != 500 {
		t.Fatalf("next=%d len=%d", next, len(lines))
	}
	if want := fmt.Sprintf("line %d", 20); lines[0][len(lines[0])-len(want):] != want {
		t.Fatalf("first kept line = %q", lines[0])
	}
	b.logf("new")
	more, next2 := b.Logs(next)
	if len(more) != 1 || next2 != 521 {
		t.Fatalf("incremental read: %v %d", more, next2)
	}
	if none, n := b.Logs(9999); len(none) != 0 || n != 521 {
		t.Fatal("reading past the end must return nothing")
	}
}

//go:build live

package xray

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveLinks measures the servers listed one per line in $AWG_LINKS_FILE (a decoded
// subscription). Run with: go test -tags live -run TestLiveLinks ./internal/xray -v
func TestLiveLinks(t *testing.T) {
	path := os.Getenv("AWG_LINKS_FILE")
	if path == "" {
		t.Skip("AWG_LINKS_FILE is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var links []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			links = append(links, l)
		}
	}
	res := TestAll(context.Background(), links, 8, 8*time.Second, nil)
	ok := 0
	for i, r := range res {
		name := links[i]
		if l, err := ParseLink(links[i]); err == nil {
			name = l.Proto + " " + l.Name
		} else {
			name = "?"
		}
		if r.Ms > 0 {
			ok++
		}
		t.Logf("%-50s %5d ms %s", name, r.Ms, r.Error)
	}
	t.Logf("работают %d из %d", ok, len(res))
}

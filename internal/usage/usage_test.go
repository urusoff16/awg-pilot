package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTotalsPersistAndSummarize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)
	s := Open(path)
	s.Add(now, 100, 10)
	s.Add(now.Add(time.Hour), 50, 5)
	s.Add(now.AddDate(0, 0, -3), 1000, 100) // same week
	s.Add(now.AddDate(0, 0, -10), 7, 7)     // same month, older than a week
	s.Add(now.AddDate(0, -2, 0), 1, 1)      // earlier month
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	sum := Open(path).Summary(now)
	if sum.Today != (Day{150, 15}) || sum.Week != (Day{1150, 115}) || sum.Month != (Day{1157, 122}) || sum.All != (Day{1158, 123}) {
		t.Fatalf("summary %+v", sum)
	}
	if len(sum.Last30) != 30 || sum.Last30[29].Date != "2026-10-31" || sum.Last30[29].Rx != 150 {
		t.Fatalf("last30 %+v", sum.Last30[29])
	}
	if sum.Since != "2026-08-31" {
		t.Fatalf("since %s", sum.Since)
	}
}

func TestOldDaysPruned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s := Open(path)
	s.Add(time.Now().AddDate(-2, 0, 0), 5, 5)
	s.Add(time.Now(), 1, 1)
	s.Save()
	if sum := Open(path).Summary(time.Now()); sum.All != (Day{1, 1}) {
		t.Fatalf("old day kept: %+v", sum.All)
	}
}

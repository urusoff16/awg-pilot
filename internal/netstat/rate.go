package netstat

import (
	"sync"
	"time"
)

// Point is one throughput sample in bytes per second.
type Point struct {
	T  int64   `json:"t"` // unix milliseconds
	Rx float64 `json:"rx"`
	Tx float64 `json:"tx"`
}

// Meter turns cumulative byte counters into per-second rates and keeps a short history.
type Meter struct {
	mu      sync.Mutex
	size    int
	last    time.Time
	lastRx  uint64
	lastTx  uint64
	history []Point
}

func NewMeter(size int) *Meter { return &Meter{size: size} }

// Add records counters observed at t. Counter resets (a new tunnel) restart the baseline.
func (m *Meter) Add(t time.Time, rx, tx uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last.IsZero() || rx < m.lastRx || tx < m.lastTx {
		m.last, m.lastRx, m.lastTx = t, rx, tx
		return
	}
	dt := t.Sub(m.last).Seconds()
	if dt <= 0 {
		return
	}
	p := Point{T: t.UnixMilli(), Rx: float64(rx-m.lastRx) / dt, Tx: float64(tx-m.lastTx) / dt}
	m.last, m.lastRx, m.lastTx = t, rx, tx
	m.history = append(m.history, p)
	if len(m.history) > m.size {
		m.history = m.history[len(m.history)-m.size:]
	}
}

// Reset forgets the baseline and history (tunnel went down).
func (m *Meter) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last, m.lastRx, m.lastTx, m.history = time.Time{}, 0, 0, nil
}

// Snapshot returns the latest point and a copy of the history.
func (m *Meter) Snapshot() (Point, []Point) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := append([]Point(nil), m.history...)
	if len(h) == 0 {
		return Point{}, h
	}
	return h[len(h)-1], h
}

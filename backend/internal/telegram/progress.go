package telegram

import (
	"sync"
	"time"
)

// Only one video downloads at a time. Speed is live state and resets on each
// attempt, including retries of the same job after an interrupted download.
type downloadProgress struct {
	mu             sync.RWMutex
	jobID          string
	sampledAt      time.Time
	sampledBytes   int64
	lastWrite      time.Time
	bytesPerSecond int64
}

func (p *downloadProgress) start(jobID string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobID = jobID
	p.sampledAt, p.lastWrite = now, now
	p.sampledBytes, p.bytesPerSecond = 0, -1
}

func (p *downloadProgress) update(jobID string, downloaded int64, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if jobID != p.jobID {
		return
	}
	if downloaded > p.sampledBytes {
		p.lastWrite = now
	}
	elapsed := now.Sub(p.sampledAt)
	if elapsed < time.Second {
		return
	}
	p.bytesPerSecond = int64(float64(max(downloaded-p.sampledBytes, 0)) / elapsed.Seconds())
	p.sampledAt, p.sampledBytes = now, downloaded
}

// A negative rate means no sample yet. Stop showing an old positive rate
// when writes have stalled, even though no new progress callback arrives.
func (p *downloadProgress) speed(jobID string, now time.Time) int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if jobID != p.jobID || p.sampledAt.IsZero() {
		return -1
	}
	if now.Sub(p.lastWrite) >= 3*time.Second {
		return 0
	}
	return p.bytesPerSecond
}

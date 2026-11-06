package telegram

import (
	"sync"
	"testing"
	"time"
)

func TestDownloadSpeedTracksSamplesStallsAndRetries(t *testing.T) {
	var progress downloadProgress
	now := time.Unix(100, 0)
	if got := progress.speed("job", now); got != -1 {
		t.Fatalf("missing download reported speed %d", got)
	}
	progress.start("job", now)
	progress.update("job", 1<<19, now.Add(500*time.Millisecond))
	if got := progress.speed("job", now.Add(500*time.Millisecond)); got != -1 {
		t.Fatalf("short sample reported speed %d", got)
	}
	progress.update("job", 1<<20, now.Add(time.Second))
	if got := progress.speed("job", now.Add(time.Second)); got != 1<<20 {
		t.Fatalf("first sample speed = %d, want 1 MiB/s", got)
	}
	progress.update("job", 5<<20, now.Add(3*time.Second))
	if got := progress.speed("job", now.Add(3*time.Second)); got != 2<<20 {
		t.Fatalf("recent speed = %d, want 2 MiB/s", got)
	}
	if got := progress.speed("job", now.Add(6*time.Second)); got != 0 {
		t.Fatalf("stalled download retained speed %d", got)
	}
	progress.update("job", 6<<20, now.Add(7*time.Second))
	if got := progress.speed("job", now.Add(7*time.Second)); got != 1<<18 {
		t.Fatalf("resumed speed = %d, want 256 KiB/s", got)
	}
	progress.start("job", now.Add(8*time.Second))
	if got := progress.speed("job", now.Add(8*time.Second)); got != -1 {
		t.Fatalf("retry retained speed %d", got)
	}
	progress.update("job", 1<<20, now.Add(9*time.Second))
	if got := progress.speed("job", now.Add(9*time.Second)); got != 1<<20 {
		t.Fatalf("retry reused the old byte count: %d", got)
	}
	progress.start("next", now.Add(10*time.Second))
	progress.update("job", 100<<20, now.Add(11*time.Second))
	if progress.speed("next", now.Add(11*time.Second)) != -1 || progress.speed("job", now.Add(11*time.Second)) != -1 {
		t.Fatal("speed leaked between jobs")
	}
}

func TestDownloadSpeedReadsDuringProgressUpdates(t *testing.T) {
	var progress downloadProgress
	now := time.Unix(100, 0)
	progress.start("job", now)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 100; i++ {
			progress.update("job", int64(i)<<20, now.Add(time.Duration(i)*time.Second))
		}
	}()
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if got := progress.speed("job", now); got != -1 && got != 1<<20 {
					t.Errorf("inconsistent speed while updating: %d", got)
				}
			}
		}()
	}
	wg.Wait()
}

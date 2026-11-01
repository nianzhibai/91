package proxy

import (
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestStreamSourceObservationsAreScopedBoundedAndExpire(t *testing.T) {
	p := New(NewRegistry())
	id := uuid.NewString()
	p.observeStreamURLSource("drive", "file", id, "quark-raw")
	if got := p.ObservedStreamURLSource("other-drive", "file", id); got != "" {
		t.Fatal("cross-drive observation")
	}
	if got := p.ObservedStreamURLSource("drive", "other-file", id); got != "" {
		t.Fatal("cross-file observation")
	}
	p.InvalidateDriveLinks("drive")
	if got := p.ObservedStreamURLSource("drive", "file", id); got != "quark-raw" {
		t.Fatal("cookie rotation lost active observation")
	}
	p.observeStreamURLSource("drive", "file", id, "quark-download")
	if got := p.ObservedStreamURLSource("drive", "file", id); got != "quark-download" {
		t.Fatal("later range request did not update provenance")
	}
	key := streamObservationKey{"drive", "file", id}
	p.observedURLSources[key] = observedURLSource{source: "quark-raw", used: time.Now().Add(-streamObservationTTL)}
	if got := p.ObservedStreamURLSource("drive", "file", id); got != "" {
		t.Fatal("expired observation retained")
	}
	p.observeStreamURLSource("drive", "file", "invalid", "quark-raw")
	if len(p.observedURLSources) != 0 {
		t.Fatal("invalid ID allocated state")
	}
	for i := 0; i < linkCacheMaxEntries+1; i++ {
		p.observeStreamURLSource("drive", "file", uuid.NewString(), "quark-raw")
	}
	if len(p.observedURLSources) != linkCacheMaxEntries {
		t.Fatal("observation cache is not bounded")
	}
	p.InvalidateDrive("drive")
	if len(p.observedURLSources) != 0 {
		t.Fatal("remount retained old account provenance")
	}
}

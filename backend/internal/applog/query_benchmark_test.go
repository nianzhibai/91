package applog

import (
	"context"
	"strings"
	"testing"
	"time"
)

func BenchmarkFilteredLogQuery(b *testing.B) {
	store, err := Open(StoreConfig{
		Directory:         b.TempDir(),
		MaxTotalSizeBytes: 64 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })

	base := time.Unix(1700000000, 0).UTC()
	payload := strings.Repeat("diagnostic context ", 8)
	for id := 1; id <= 30000; id++ {
		rec := Record{
			Timestamp: base.Add(time.Duration(id) * time.Second),
			Source:    SourceHTTP,
			Level:     LevelInfo,
			RequestID: "1b0e4c339cae75f3d37aa989e8cf6007",
			HTTP: &HTTPRecord{
				Method:     MethodGET,
				Path:       "/api/settings/preview",
				Route:      "/api/settings/preview",
				Status:     200,
				Bytes:      24,
				DurationMs: 0.35,
				Remote:     "192.0.2.1",
			},
		}
		if id%100 == 0 {
			rec.Source = SourceApp
			rec.Level = LevelError
			rec.Component = "scan"
			rec.DriveID = "drive-1"
			rec.HTTP = nil
			rec.Message = "scan failed: " + payload
		}
		store.Submit(rec)
	}
	if err := store.Flush(context.Background()); err != nil {
		b.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		query Query
	}{
		{"Latest1000", Query{Source: SourceHTTP, Limit: 1000}},
		{"RareSource", Query{Source: SourceApp, Limit: 1000}},
		{"IndexedComponentAndDrive", Query{Component: "scan", DriveID: "drive-1", Limit: 100}},
		{"NoMatchingLevel", Query{Levels: []Level{LevelWarn}, Limit: 1000}},
		{"FTS5NoMatch", Query{Search: "not-present-in-logs", Limit: 1000}},
		{"FTS5Match", Query{Search: "diagnostic", Limit: 100}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := store.Query(context.Background(), tc.query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

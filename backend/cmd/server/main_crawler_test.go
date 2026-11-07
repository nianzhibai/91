package main

import (
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestCrawlerIntCredFallbacks(t *testing.T) {
	tests := []struct {
		name string
		d    *catalog.Drive
		key  string
		def  int
		want int
	}{
		{"nil drive", nil, "page", 1, 1},
		{"nil creds", &catalog.Drive{}, "page", 7, 7},
		{"empty value", &catalog.Drive{Credentials: map[string]string{"page": ""}}, "page", 5, 5},
		{"non-numeric", &catalog.Drive{Credentials: map[string]string{"page": "abc"}}, "page", 9, 9},
		{"happy", &catalog.Drive{Credentials: map[string]string{"page": "42"}}, "page", 1, 42},
		{"missing key", &catalog.Drive{Credentials: map[string]string{"a": "1"}}, "b", 99, 99},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := crawlerIntCred(tc.d, tc.key, tc.def)
			if got != tc.want {
				t.Fatalf("crawlerIntCred(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

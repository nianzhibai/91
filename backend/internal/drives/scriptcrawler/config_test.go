package scriptcrawler

import "testing"

func TestIsConfiguredRequiresScriptPath(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string]string
		want        bool
	}{
		{name: "nil credentials", want: false},
		{name: "missing script path", credentials: map[string]string{"upload_drive_id": "pikpak"}, want: false},
		{name: "blank script path", credentials: map[string]string{"script_path": "  \t"}, want: false},
		{name: "configured", credentials: map[string]string{"script_path": "/tmp/crawler.py"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsConfigured(test.credentials); got != test.want {
				t.Fatalf("IsConfigured() = %v, want %v", got, test.want)
			}
		})
	}
}

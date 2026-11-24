package drives

import "testing"

func TestCapabilitiesForKind(t *testing.T) {
	for _, kind := range []string{"quark", "p115", "p123", "pikpak", "wopan", "yun139", "guangyapan", "onedrive", "googledrive", "webdav"} {
		if !CapabilitiesForKind(kind).Upload {
			t.Fatalf("kind %q should advertise upload", kind)
		}
	}
	for _, kind := range []string{"localstorage", "local-upload", "scriptcrawler", "unknown"} {
		if CapabilitiesForKind(kind).Upload {
			t.Fatalf("kind %q should not advertise upload", kind)
		}
	}
}

func TestDefaultGenerationParallelismLeavesLocalSourcesToTheGlobalLimit(t *testing.T) {
	for _, kind := range []string{"local-upload", "telegram-local", "localstorage", "scriptcrawler"} {
		if got := DefaultGenerationParallelism(kind); got != 0 {
			t.Errorf("%s = %d, want the global limit", kind, got)
		}
	}
	for _, kind := range []string{"p115", "quark", "webdav", "pikpak", "onedrive"} {
		if got := DefaultGenerationParallelism(kind); got != 1 {
			t.Errorf("%s = %d, want one video at a time", kind, got)
		}
	}
}

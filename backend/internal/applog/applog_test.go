package applog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactionPreservesDiagnosticContextAndProtectsSecrets(t *testing.T) {
	for _, input := range []string{
		`GET https://alice:private-password@example.com/path?deviceCode=private-code&loginUuid=private-uuid&dir=42`,
		`Authorization: Bearer private-token`,
		`Cookie: session=private-session; other=private-cookie`,
		`{"access_token":"private-access","refresh_token":"private-refresh","password":"private-pass"}`,
		`GET /api/status?token=private-token&dir=42`,
		`request failed: Get "https://example.com/path?signature=private-signature": deadline exceeded`,
		`https://example.com/file?X-Amz-Signature=private-signature&X-Amz-Credential=private-credential&auth_key=private-auth`,
		`{"code":500,"nested":{"clientSecret":"private-client","\u0074oken":"private-escaped"}}`,
		`https://api.telegram.org/bot123456789:AAHprivateToken_abc-123/getUpdates`,
		`request /bot987654:BBHprivateToken/sendMessage failed`,
		`bot_token=private-bot-token`,
		`sign=private-sig`,
	} {
		got := Redact(input)
		if strings.Contains(got, "private") {
			t.Fatalf("secret remained in %q -> %q", input, got)
		}
	}
	// Verify Telegram URL keeps method name while hiding token.
	tg := RedactURL("https://api.telegram.org/bot123456:AAHsecret_xyz/getMe?timeout=30")
	if strings.Contains(tg, "AAHsecret") || !strings.Contains(tg, "/botREDACTED/getMe") || !strings.Contains(tg, "timeout=30") {
		t.Fatalf("telegram URL redaction = %q", tg)
	}
	// Verify ordinary words containing "sign" are NOT corrupted.
	for _, safe := range []string{
		"theme design=dark assign=1",
		"[scanner] drive=drive-1 file=42: permission denied",
		`{"code":500,"id":12345678901234567890}`,
	} {
		if got := Redact(safe); got != safe {
			t.Fatalf("safe text was altered: %q -> %q", safe, got)
		}
	}
}

func TestHandlerLevelHotReloadAndContextFields(t *testing.T) {
	var console bytes.Buffer
	rt, err := OpenRuntime(RuntimeConfig{
		Directory:     t.TempDir(),
		Level:         LevelInfo,
		ConsoleFormat: ConsoleFormatJSON,
		ConsoleWriter: &console,
		FileEnabled:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	ctx := WithFields(context.Background(), Fields{RequestID: "req-1"})
	ctx = NewTask(ctx, "scan", "drive-1")
	ctx = WithFields(ctx, Fields{FileID: "file-1", Stage: "discover"})

	// Debug suppressed at Info level.
	rt.Logger().DebugContext(ctx, "hidden debug", "attempt", 1)
	rt.Logger().InfoContext(ctx, "scan finished", "scanned", 12, "token", "private-secret")

	// Hot-reload to Debug.
	rt.SetLevel(LevelDebug)
	rt.Logger().DebugContext(ctx, "visible debug", "attempt", 2)

	snap, err := rt.Store().Query(context.Background(), Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(snap.Entries), snap.Entries)
	}
	first := snap.Entries[0]
	if first.Level != LevelInfo || first.Component != "scan" || first.RequestID != "req-1" ||
		first.DriveID != "drive-1" || first.FileID != "file-1" || first.Stage != "discover" || first.TaskID == "" {
		t.Fatalf("first entry = %+v", first)
	}
	if first.Attrs["scanned"] != float64(12) || first.Attrs["token"] != redacted {
		t.Fatalf("first attrs = %+v", first.Attrs)
	}
	second := snap.Entries[1]
	if second.Level != LevelDebug || second.Message != "visible debug" {
		t.Fatalf("second entry = %+v", second)
	}
	if strings.Contains(console.String(), "private-secret") {
		t.Fatalf("console output leaked secret: %s", console.String())
	}
	lines := strings.Split(strings.TrimSpace(console.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("console JSON lines = %d, want 2", len(lines))
	}
	var decoded Record
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("invalid console JSON: %v", err)
	}
	if decoded.ID != first.ID {
		t.Fatalf("console ID %d != stored ID %d", decoded.ID, first.ID)
	}
}

func TestCanceledErrorDowngradesToWarn(t *testing.T) {
	rt, err := OpenRuntime(RuntimeConfig{
		Directory:     t.TempDir(),
		Level:         LevelInfo,
		ConsoleWriter: &bytes.Buffer{},
		FileEnabled:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	prev := slog.Default()
	slog.SetDefault(rt.Logger())
	defer slog.SetDefault(prev)

	ctx := WithFields(context.Background(), Fields{Component: "scan", TaskID: "task-cancel"})
	Error(ctx, "task interrupted", fmt.Errorf("upstream: %w", context.Canceled))

	snap, err := rt.Store().Query(context.Background(), Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Level != LevelWarn || snap.Entries[0].TaskID != "task-cancel" {
		t.Fatalf("entry = %+v", snap.Entries)
	}
}

func TestHTTPLogAndPanicRecord(t *testing.T) {
	var console bytes.Buffer
	rt, err := OpenRuntime(RuntimeConfig{
		Directory:     t.TempDir(),
		Level:         LevelInfo,
		ConsoleFormat: ConsoleFormatText,
		ConsoleWriter: &console,
		FileEnabled:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	prev := slog.Default()
	slog.SetDefault(rt.Logger())
	defer slog.SetDefault(prev)

	ctx := WithFields(context.Background(), Fields{RequestID: "req-http-1"})
	LogHTTP(ctx, LevelWarn, HTTPRecord{
		Method:     MethodGET,
		Path:       "/api/test?token=private-token&dir=1",
		Route:      "/api/test",
		Status:     404,
		Bytes:      19,
		DurationMs: 12.34,
		Remote:     "203.0.113.25",
	}, "")
	LogPanic(ctx, &HTTPRecord{Method: MethodPOST, Path: "/api/boom?secret=private-s"}, "kaboom", []byte("goroutine 1:\nmain.go:42"))

	snap, err := rt.Store().Query(context.Background(), Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 2 {
		t.Fatalf("entries = %+v", snap.Entries)
	}
	httpEntry := snap.Entries[0]
	if httpEntry.Source != SourceHTTP || httpEntry.Level != LevelWarn || httpEntry.HTTP == nil ||
		httpEntry.HTTP.Status != 404 || httpEntry.HTTP.DurationMs != 12.34 ||
		strings.Contains(httpEntry.HTTP.Path, "private-token") || !strings.Contains(httpEntry.HTTP.Path, "dir=1") {
		t.Fatalf("http entry = %+v", httpEntry)
	}
	panicEntry := snap.Entries[1]
	if panicEntry.Level != LevelError || !strings.Contains(panicEntry.Stack, "goroutine 1") ||
		panicEntry.RequestID != "req-http-1" || strings.Contains(panicEntry.HTTP.Path, "private-s") {
		t.Fatalf("panic entry = %+v", panicEntry)
	}
	if strings.Contains(console.String(), "private-") {
		t.Fatalf("console leaked secret: %s", console.String())
	}
}

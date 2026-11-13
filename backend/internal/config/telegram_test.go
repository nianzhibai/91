package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTelegramConfigValidation(t *testing.T) {
	for _, input := range []string{
		"telegram:\n  bot_token: secret\n",
		"telegram:\n  enabled: true\n",
		"telegram:\n  max_pending_jobs: -1\n",
		"telegram:\n  download_threads: -1\n",
		"telegram:\n  download_threads: 9\n",
		"telegram:\n  download_threads: 1.5\n",
		"telegram:\n  download_threads: '6'\n",
		"telegram:\n  allowed_user_ids: [-1]\n",
		"telegram:\n  fetch_timeout_seconds: 1\n",
		"telegram:\n  upload_directory: ../outside\n",
		"telegram:\n  upload_directory: /absolute\n",
		"telegram:\n  upload_directory: 'Telegram\\videos'\n",
		"telegram:\n  upload_drive_id: local-upload\n",
		"telegram:\n  upload_proxy: ftp://proxy.example:21\n",
		"telegram:\n  upload_proxy: proxy.example:7890\n",
		"telegram:\n  upload_proxy: http://\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	cfg, err := Parse([]byte("telegram:\n  enabled: true\n  api_id: 12345\n  api_hash: 0123456789abcdef0123456789abcdef\n  bot_token: 123:test_token\n  allowed_user_ids: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.MaxFileSizeBytes != 4<<30 || cfg.Telegram.DownloadThreads != 6 || cfg.Telegram.UploadDriveID != "" || cfg.Telegram.UploadDirectory != "Telegram" || cfg.Telegram.UploadProxy != "" {
		t.Fatalf("defaults: %+v", cfg.Telegram)
	}
}

func TestTelegramDownloadThreads(t *testing.T) {
	for _, threads := range []int{0, 1, 6, 8} {
		cfg, err := Parse([]byte(fmt.Sprintf("telegram:\n  download_threads: %d\n", threads)))
		if err != nil {
			t.Fatal(err)
		}
		expected := threads
		if expected == 0 {
			expected = 6
		}
		if cfg.Telegram.DownloadThreads != expected {
			t.Fatalf("download threads = %d, want %d", cfg.Telegram.DownloadThreads, expected)
		}
	}
}

func TestTelegramUploadProxyValidationAndRedaction(t *testing.T) {
	for _, proxy := range []string{"", "http://proxy.example:7890", "https://user:private_password@proxy.example:443", "socks5://proxy.example:1080", "socks5h://proxy.example:1080"} {
		cfg, err := Parse([]byte("telegram:\n  upload_proxy: '  " + proxy + "  '\n"))
		if err != nil || cfg.Telegram.UploadProxy != proxy {
			t.Fatalf("proxy did not normalize: %v", err)
		}
		data, err := json.Marshal(cfg.Telegram)
		if err != nil || strings.Contains(string(data), "proxy") || strings.Contains(string(data), "private_password") {
			t.Fatal("status JSON exposed upload proxy", err)
		}
	}
	if _, err := Parse([]byte("telegram:\n  upload_proxy: 'http://user:private_password@proxy.example:invalid'\n")); err == nil || strings.Contains(err.Error(), "private_password") {
		t.Fatalf("expected sanitized validation error: %v", err)
	}
}

func TestTelegramApplicationCredentialsValidation(t *testing.T) {
	for _, cfg := range []Telegram{
		{APIID: -1}, {APIID: 2147483648}, {APIHash: "invalid"},
		{Enabled: true, APIID: 1, BotToken: "123:token"},
		{Enabled: true, APIHash: "0123456789abcdef0123456789abcdef", BotToken: "123:token"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatal("invalid Telegram credentials accepted")
		}
	}
	cfg := Telegram{Enabled: true, APIID: 123, APIHash: " 0123456789abcdef0123456789abcdef ", BotToken: "123:token"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(raw), "0123456789") || strings.Contains(string(raw), "123:token") {
		t.Fatal("status exposed credentials")
	}
}

func TestTelegramConnectionProxyValidationAndRedaction(t *testing.T) {
	for _, proxy := range []string{"", "http://proxy.example:7890", "https://user:PRIVATE_PASSWORD@proxy.example", "socks5://proxy.example:1080", "socks5h://user:p%40ss@proxy.example:1080"} {
		cfg, err := Parse([]byte("telegram:\n  connection_proxy: '  " + proxy + "  '\n"))
		if err != nil || cfg.Telegram.ConnectionProxy != proxy {
			t.Fatalf("proxy did not normalize: %v", err)
		}
		raw, err := json.Marshal(cfg.Telegram)
		if err != nil || strings.Contains(string(raw), "proxy") || strings.Contains(string(raw), "PRIVATE_PASSWORD") {
			t.Fatal("status exposed connection proxy", err)
		}
	}
	for _, proxy := range []string{"ftp://user:PRIVATE_PASSWORD@proxy.example", "proxy.example:1080", "http://", "socks5://proxy.example:0", "http://proxy.example:7890/path"} {
		if _, err := Parse([]byte("telegram:\n  connection_proxy: '" + proxy + "'\n")); err == nil || strings.Contains(err.Error(), "PRIVATE_PASSWORD") {
			t.Fatalf("invalid or unsafe proxy error: %v", err)
		}
	}
}

func TestLegacyBotAPITelegramLoadsDisabled(t *testing.T) {
	cfg, err := Parse([]byte("telegram:\n  enabled: true\n  bot_token: 123:legacy_token\n  allowed_user_ids: [42]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.Enabled || cfg.Telegram.BotToken != "123:legacy_token" || len(cfg.Telegram.AllowedUserIDs) != 1 {
		t.Fatalf("legacy Telegram settings: %+v", cfg.Telegram)
	}
	// Current configurations always contain the credential fields and stay strict.
	if _, err := Parse([]byte("telegram:\n  enabled: true\n  api_id: 0\n  api_hash: \"\"\n  bot_token: 123:token\n")); err == nil {
		t.Fatal("accepted enabled Telegram without credentials")
	}
}

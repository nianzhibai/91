package applog

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzRedactionFastPathMatchesFullRules(f *testing.F) {
	for _, input := range []string{
		"", "worker ready", "1b0e4c339cae75f3d37aa989e8cf6007",
		"theme design=dark assign=1",
		`GET http://example.test:9191/api/settings/preview HTTP/1.1`,
		"GET http://example.test/api HTTP/1.1 - 200 24B in 35\u00b5s",
		`Authorization: Bearer private-token`, `Proxy-Authorization: Basic private-token`,
		`Cookie: session=private-session; other=private-cookie`, `Set-Cookie: private-cookie`,
		`Basic private-token`, `Bearer private-token`,
		`https://user:private-pass@example.test/path?code=private-code&dir=42`,
		`https://api.telegram.org/bot12345:AAHprivate_token/getMe`,
		`GET https://example.test/path?token=private-token`,
		"\u017fign=private-signature", "api_\u212aey=private-key",
		"\u9519\u8bef password=private-pass", "PASSWORD : \"private-pass\"\nCookie: private-cookie",
	} {
		f.Add(input)
	}
	for _, key := range append(append([]string{}, compoundSecretKeys...), boundedSecretKeys...) {
		f.Add(strings.Repeat("diagnostic context ", 20) + key + "=private-value")
		f.Add(strings.ToUpper(key) + `: "private-value"`)
	}
	f.Fuzz(func(t *testing.T, input string) {
		trimmed := strings.TrimSpace(input)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
			return // Structured JSON uses its own parser before the text fast path.
		}
		want := logURL.ReplaceAllStringFunc(input, func(raw string) string {
			urlPart := strings.TrimRight(raw, "),;.")
			return RedactURL(urlPart) + strings.TrimPrefix(raw, urlPart)
		})
		want = telegramBotPath.ReplaceAllString(want, "${1}"+urlRedacted+"${2}")
		want = authHeader.ReplaceAllString(want, "${1}"+redacted)
		want = authValue.ReplaceAllString(want, "${1} "+redacted)
		want = secretValue.ReplaceAllString(want, "${1}"+redacted)
		if got := Redact(input); got != want {
			t.Fatalf("fast redaction %q differs from full rules %q for %q", got, want, input)
		}
	})
}

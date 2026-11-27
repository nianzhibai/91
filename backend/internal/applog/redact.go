package applog

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

const (
	redacted    = "[REDACTED]"
	urlRedacted = "REDACTED"
)

var (
	// Unambiguous credential stems may appear inside compound identifiers
	// (e.g., bot_token, clientSecret, X-Amz-Signature).
	compoundSecretKeys = []string{
		"password", "passwd", "secret", "token", "credential",
		"api_key", "apikey", "access_key", "accesskey",
		"cookie", "devicecode", "device_code", "loginuuid", "login_uuid",
		"signature", "auth_key", "authkey", "session_id", "sessionid",
	}
	// Short stems require a non-alphanumeric boundary so ordinary words like
	// "design=dark" or "assign=1" are not corrupted.
	boundedSecretKeys = []string{
		"sign", "authorization",
	}
	fastPathSecretNeedles = append(append([]string{"bearer", "basic", "/bot"}, compoundSecretKeys...), boundedSecretKeys...)

	logURL          = regexp.MustCompile(`https?://[^\s<>"']+`)
	telegramBotPath = regexp.MustCompile(`(?i)(/bot)\d+:[a-z0-9_-]+(/|$)`)
	authHeader      = regexp.MustCompile(`(?im)(\b(?:authorization|proxy-authorization|cookie|set-cookie)\s*:\s*)[^\r\n]+`)
	authValue       = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[a-z0-9._~+/=-]+`)
	secretValue     = regexp.MustCompile(
		`(?i)((?:[a-z0-9_.-]*(?:` + strings.Join(compoundSecretKeys, "|") + `)|(?:^|[^a-z0-9])(?:` + strings.Join(boundedSecretKeys, "|") + `))["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&}]+)`,
	)
)

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
	return strings.Contains(key, "token") || strings.Contains(key, "password") || strings.Contains(key, "secret") ||
		strings.Contains(key, "credential") || strings.Contains(key, "cookie") ||
		strings.Contains(key, "signature") || strings.Contains(key, "accesskey") ||
		key == "passwd" || key == "authorization" || key == "apikey" || key == "key" ||
		key == "authkey" || key == "auth" || key == "sessionid" || key == "session" ||
		key == "sign" || key == "devicecode" || key == "loginuuid" || key == "code"
}

func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	u.User = nil
	u.Fragment = ""
	if strings.Contains(strings.ToLower(u.Path), "/bot") {
		u.Path = telegramBotPath.ReplaceAllString(u.Path, "${1}"+urlRedacted+"${2}")
		u.RawPath = ""
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		u.RawQuery = "redacted=invalid-query"
		return u.String()
	}
	changed := false
	for key := range values {
		if sensitiveKey(key) {
			values.Set(key, redacted)
			changed = true
		}
	}
	if changed {
		u.RawQuery = values.Encode()
	}
	return u.String()
}

func Redact(text string) string {
	if text == "" {
		return text
	}
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
		var value any
		decoder := json.NewDecoder(strings.NewReader(trimmed))
		decoder.UseNumber()
		if decoder.Decode(&value) == nil {
			encoded, err := json.Marshal(redactJSON(value))
			if err == nil {
				return string(encoded)
			}
		}
	}
	if strings.Contains(text, "://") {
		text = logURL.ReplaceAllStringFunc(text, func(raw string) string {
			urlPart := strings.TrimRight(raw, "),;.")
			return RedactURL(urlPart) + strings.TrimPrefix(raw, urlPart)
		})
	}
	if !mayContainLogSecret(text) {
		return text
	}
	text = telegramBotPath.ReplaceAllString(text, "${1}"+urlRedacted+"${2}")
	text = authHeader.ReplaceAllString(text, "${1}"+redacted)
	text = authValue.ReplaceAllString(text, "${1} "+redacted)
	return secretValue.ReplaceAllString(text, "${1}"+redacted)
}

func mayContainLogSecret(text string) bool {
	// Match regexp's ASCII-key case folding without scanning ordinary text with regexps.
	lower := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII {
			for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
				if folded <= unicode.MaxASCII {
					return unicode.ToLower(folded)
				}
			}
		}
		return unicode.ToLower(r)
	}, text)
	for _, needle := range fastPathSecretNeedles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func redactJSON(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if sensitiveKey(key) {
				// Numeric provider error codes are diagnostic, not OAuth codes.
				if strings.EqualFold(key, "code") {
					if _, ok := child.(json.Number); ok {
						continue
					}
				}
				value[key] = redacted
			} else {
				value[key] = redactJSON(child)
			}
		}
	case []any:
		for i, child := range value {
			value[i] = redactJSON(child)
		}
	case string:
		return Redact(value)
	}
	return value
}

func redactAttrValue(key string, value any) any {
	if sensitiveKey(key) {
		if strings.EqualFold(key, "code") {
			switch value.(type) {
			case int, int64, uint64, float64, json.Number:
				return value
			}
		}
		return redacted
	}
	switch v := value.(type) {
	case string:
		return Redact(v)
	case map[string]any:
		cp := make(map[string]any, len(v))
		for k, child := range v {
			cp[k] = redactAttrValue(k, child)
		}
		return cp
	case []any:
		cp := make([]any, len(v))
		for i, child := range v {
			cp[i] = redactAttrValue("", child)
		}
		return cp
	default:
		return value
	}
}

func redactRecord(rec *Record) {
	rec.Message = Redact(rec.Message)
	rec.Error = Redact(rec.Error)
	rec.Stack = Redact(rec.Stack)
	rec.Component = Redact(rec.Component)
	rec.RequestID = Redact(rec.RequestID)
	rec.TaskID = Redact(rec.TaskID)
	rec.DriveID = Redact(rec.DriveID)
	rec.VideoID = Redact(rec.VideoID)
	rec.FileID = Redact(rec.FileID)
	rec.Stage = Redact(rec.Stage)
	if rec.HTTP != nil && rec.HTTP.Path != "" {
		rec.HTTP.Path = RedactURL(rec.HTTP.Path)
	}
	if len(rec.Attrs) > 0 {
		for k, v := range rec.Attrs {
			rec.Attrs[k] = redactAttrValue(k, v)
		}
	}
}

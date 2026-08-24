package pikpak

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var androidAlgorithms = []string{
	"SOP04dGzk0TNO7t7t9ekDbAmx+eq0OI1ovEx",
	"nVBjhYiND4hZ2NCGyV5beamIr7k6ifAsAbl",
	"Ddjpt5B/Cit6EDq2a6cXgxY9lkEIOw4yC1GDF28KrA",
	"VVCogcmSNIVvgV6U+AochorydiSymi68YVNGiz",
	"u5ujk5sM62gpJOsB/1Gu/zsfgfZO",
	"dXYIiBOAHZgzSruaQ2Nhrqc2im",
	"z5jUTBSIpBN9g4qSJGlidNAutX6",
	"KJE2oveZ34du/g1tiimm",
}

var webAlgorithms = []string{
	"C9qPpZLN8ucRTaTiUMWYS9cQvWOE",
	"+r6CQVxjzJV6LCV",
	"F",
	"pFJRC",
	"9WXYIDGrwTCz2OiVlgZa90qpECPD6olt",
	"/750aCr4lm/Sly/c",
	"RB+DT/gZCrbV",
	"",
	"CyLsf7hdkIRxRm215hl",
	"7xHvLi2tOYP0Y92b",
	"ZGTXXxu8E/MIWaEDB+Sm/",
	"1UI3",
	"E7fP5Pfijd+7K+t6Tg/NhuLq0eEUVChpJSkrKxpO",
	"ihtqpG6FMt65+Xk+tWUH2",
	"NhXXU9rg4XXdzo7u5o",
}

var pcAlgorithms = []string{
	"KHBJ07an7ROXDoK7Db",
	"G6n399rSWkl7WcQmw5rpQInurc1DkLmLJqE",
	"JZD1A3M4x+jBFN62hkr7VDhkkZxb9g3rWqRZqFAAb",
	"fQnw/AmSlbbI91Ik15gpddGgyU7U",
	"/Dv9JdPYSj3sHiWjouR95NTQff",
	"yGx2zuTjbWENZqecNI+edrQgqmZKP",
	"ljrbSzdHLwbqcRn",
	"lSHAsqCkGDGxQqqwrVu",
	"TsWXI81fD1",
	"vk7hBjawK/rOSrSWajtbMk95nfgf3",
}

func (d *Driver) applyPlatformDefaults() {
	switch d.platform {
	case "android":
		d.clientID = "YNxT9w7GMdWvEOKa"
		d.clientSecret = "dbw2OtmVEeuUvIptb1Coyg"
		d.clientVersion = "1.53.2"
		d.packageName = "com.pikcloud.pikpak"
		d.algorithms = androidAlgorithms
		d.userAgent = buildAndroidUserAgent(d.deviceID, d.clientID, d.packageName, "2.0.6.206003", d.clientVersion, d.packageName, d.userID)
	case "pc":
		d.clientID = "YvtoWO6GNHiuCl7x"
		d.clientSecret = "1NIH5R1IEe2pAxZE3hv3uA"
		d.clientVersion = "undefined"
		d.packageName = "mypikpak.com"
		d.algorithms = pcAlgorithms
		d.userAgent = "MainWindow Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) PikPak/2.6.11.4955 Chrome/100.0.4896.160 Electron/18.3.15 Safari/537.36"
	default:
		d.platform = "web"
		d.clientID = "YUMx5nI8ZU8Ap8pm"
		d.clientSecret = "dbw2OtmVEeuUvIptb1Coyg"
		d.clientVersion = "2.0.0"
		d.packageName = "mypikpak.com"
		d.algorithms = webAlgorithms
		d.userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Safari/537.36"
	}
}

func (d *Driver) login(ctx context.Context) error {
	if d.username == "" || d.password == "" {
		return fmt.Errorf("pikpak username or password is empty")
	}
	action := getAction(http.MethodPost, signinURL)
	if d.authSnapshot().captchaToken == "" {
		if err := d.refreshCaptchaTokenInLogin(ctx, action, d.username); err != nil {
			return err
		}
	}
	if err := d.signinOnce(ctx); err != nil {
		if !IsCaptchaError(err) {
			return err
		}
		// captcha_token is short-lived, but it is persisted so the same device can
		// resume after restart. If signin rejects that stored token, clear and
		// persist the cleared state before acquiring a replacement. Retry signin
		// exactly once so a provider-side captcha outage cannot become a loop.
		d.clearCaptchaToken()
		if refreshErr := d.refreshCaptchaTokenInLogin(ctx, action, d.username); refreshErr != nil {
			return fmt.Errorf("pikpak signin captcha recovery: %w", refreshErr)
		}
		if retryErr := d.signinOnce(ctx); retryErr != nil {
			return fmt.Errorf("pikpak signin after captcha refresh: %w", retryErr)
		}
	}
	return nil
}

func (d *Driver) signinOnce(ctx context.Context) error {
	auth := d.authSnapshot()
	var out authResp
	var e errResp
	res, err := d.client.R().
		SetContext(ctx).
		SetError(&e).
		SetResult(&out).
		SetQueryParam("client_id", d.clientID).
		SetBody(map[string]any{
			"captcha_token": auth.captchaToken,
			"client_id":     d.clientID,
			"client_secret": d.clientSecret,
			"username":      d.username,
			"password":      d.password,
		}).
		Post(signinURL)
	if err != nil {
		return err
	}
	if e.isError() {
		if e.ErrorCode == 4126 {
			return fmt.Errorf("pikpak login: 密码登录被 PikPak 风控拦截，请配置 refresh_token 或改用 WebDAV: %w", &e)
		}
		return &e
	}
	if res.IsError() {
		return fmt.Errorf("pikpak signin http %d: %s", res.StatusCode(), string(res.Body()))
	}
	if strings.TrimSpace(out.AccessToken) == "" || strings.TrimSpace(out.RefreshToken) == "" {
		return fmt.Errorf("pikpak signin: empty access_token or refresh_token")
	}
	d.applyAuth(out)
	return nil
}

func (d *Driver) refresh(ctx context.Context, rejected ...authStateSnapshot) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	auth := d.authSnapshot()
	if len(rejected) > 0 &&
		(auth.tokenGeneration != rejected[0].tokenGeneration || auth.accessToken != rejected[0].accessToken) {
		return nil
	}
	if auth.refreshToken == "" {
		return fmt.Errorf("pikpak refresh_token is empty")
	}
	var out authResp
	var e errResp
	res, err := d.client.R().
		SetContext(ctx).
		SetHeader("User-Agent", "").
		SetError(&e).
		SetResult(&out).
		SetQueryParam("client_id", d.clientID).
		SetBody(map[string]any{
			"client_id":     d.clientID,
			"client_secret": d.clientSecret,
			"grant_type":    "refresh_token",
			"refresh_token": auth.refreshToken,
		}).
		Post(tokenURL)
	if err != nil {
		return err
	}
	if e.isError() {
		if e.ErrorCode == 4126 && d.username != "" && d.password != "" {
			return d.login(ctx)
		}
		return &e
	}
	if res.IsError() {
		return fmt.Errorf("pikpak refresh http %d: %s", res.StatusCode(), string(res.Body()))
	}
	if strings.TrimSpace(out.AccessToken) == "" || strings.TrimSpace(out.RefreshToken) == "" {
		return fmt.Errorf("pikpak refresh: empty access_token or refresh_token")
	}
	d.applyAuth(out)
	return nil
}

func (d *Driver) applyAuth(out authResp) {
	d.authUpdateMu.Lock()
	defer d.authUpdateMu.Unlock()

	d.authMu.Lock()
	d.accessToken = strings.TrimSpace(out.AccessToken)
	d.refreshToken = strings.TrimSpace(out.RefreshToken)
	d.userID = strings.TrimSpace(out.Sub)
	d.tokenGeneration++
	if d.platform == "android" {
		d.userAgent = buildAndroidUserAgent(d.deviceID, d.clientID, d.packageName, "2.0.6.206003", d.clientVersion, d.packageName, d.userID)
	}
	auth := d.authSnapshotLocked()
	d.authMu.Unlock()
	d.persistAuthSnapshot(auth)
}

func (d *Driver) persistTokens() {
	d.authUpdateMu.Lock()
	defer d.authUpdateMu.Unlock()
	d.persistAuthSnapshot(d.authSnapshot())
}

func (d *Driver) persistAuthSnapshot(auth authStateSnapshot) {
	if d.onTokenUpdate != nil {
		d.onTokenUpdate(auth.accessToken, auth.refreshToken, auth.captchaToken, d.deviceID)
	}
}

func (d *Driver) clearCaptchaToken() {
	d.authUpdateMu.Lock()
	defer d.authUpdateMu.Unlock()

	d.authMu.Lock()
	if d.captchaToken == "" {
		d.authMu.Unlock()
		return
	}
	d.captchaToken = ""
	auth := d.authSnapshotLocked()
	d.authMu.Unlock()
	d.persistAuthSnapshot(auth)
}

func (d *Driver) setCaptchaToken(token string) {
	d.authUpdateMu.Lock()
	defer d.authUpdateMu.Unlock()

	d.authMu.Lock()
	d.captchaToken = strings.TrimSpace(token)
	auth := d.authSnapshotLocked()
	d.authMu.Unlock()
	d.persistAuthSnapshot(auth)
}

func (d *Driver) refreshCaptchaTokenAtLogin(ctx context.Context, action, userID string) error {
	return d.refreshCaptchaToken(ctx, action, d.captchaTokenAtLoginMeta(userID))
}

func (d *Driver) recoverCaptchaTokenAtLogin(ctx context.Context, action, userID, rejectedToken string) error {
	d.captchaMu.Lock()
	defer d.captchaMu.Unlock()

	current := d.authSnapshot()
	if current.captchaToken != rejectedToken {
		return nil
	}
	if current.captchaToken != "" {
		d.clearCaptchaToken()
	}
	if current.userID != "" {
		userID = current.userID
	}
	return d.refreshCaptchaTokenOnce(ctx, action, d.captchaTokenAtLoginMeta(userID), true)
}

func (d *Driver) captchaTokenAtLoginMeta(userID string) map[string]string {
	timestamp, sign := d.captchaSign()
	return map[string]string{
		"client_version": d.clientVersion,
		"package_name":   d.packageName,
		"user_id":        userID,
		"timestamp":      timestamp,
		"captcha_sign":   sign,
	}
}

func (d *Driver) refreshCaptchaTokenInLogin(ctx context.Context, action, username string) error {
	meta := make(map[string]string)
	if ok, _ := regexp.MatchString(`\w+([-+.]\w+)*@\w+([-.]\w+)*\.\w+([-.]\w+)*`, username); ok {
		meta["email"] = username
	} else if len(username) >= 11 && len(username) <= 18 {
		meta["phone_number"] = username
	} else {
		meta["username"] = username
	}
	return d.refreshCaptchaToken(ctx, action, meta)
}

func (d *Driver) refreshCaptchaToken(ctx context.Context, action string, meta map[string]string) error {
	d.captchaMu.Lock()
	defer d.captchaMu.Unlock()
	return d.refreshCaptchaTokenOnce(ctx, action, meta, true)
}

// refreshCaptchaTokenOnce 调 /v1/shield/captcha/init 申请新 captcha token。
//
// 如果 retry=true 且服务端返回 captcha 失效错误（4002 或 9），就清空缓存的
// captcha_token 后再调一次；这次 body 里 captcha_token 为空，服务端会直接发一个新的。这覆盖
// driver 重启后 Init() 用持久化的旧 captcha_token 调 captcha init 失败的
// 场景。
func (d *Driver) refreshCaptchaTokenOnce(ctx context.Context, action string, meta map[string]string, retry bool) error {
	auth := d.authSnapshot()
	var e errResp
	var out captchaTokenResponse
	req := d.client.R().
		SetContext(ctx).
		SetHeader("User-Agent", auth.userAgent).
		SetHeader("X-Device-ID", d.deviceID).
		SetError(&e).
		SetResult(&out).
		SetQueryParam("client_id", d.clientID).
		SetBody(captchaTokenRequest{
			Action:       action,
			CaptchaToken: auth.captchaToken,
			ClientID:     d.clientID,
			DeviceID:     d.deviceID,
			Meta:         meta,
			RedirectURI:  "xlaccsdk01://xbase.cloud/callback?state=harbor",
		})
	if auth.accessToken != "" {
		req.SetHeader("Authorization", "Bearer "+auth.accessToken)
	}
	res, err := req.Post(captchaInitURL)
	if err != nil {
		return err
	}
	if e.isError() {
		if retry && isCaptchaTokenRejectedCode(e.ErrorCode) && auth.captchaToken != "" {
			d.clearCaptchaToken()
			return d.refreshCaptchaTokenOnce(ctx, action, meta, false)
		}
		return &e
	}
	if res.IsError() {
		return fmt.Errorf("pikpak captcha http %d: %s", res.StatusCode(), string(res.Body()))
	}
	if out.URL != "" {
		return fmt.Errorf("pikpak captcha verification required: %s", out.URL)
	}
	d.setCaptchaToken(out.CaptchaToken)
	return nil
}

func (d *Driver) captchaSign() (timestamp, sign string) {
	timestamp = fmt.Sprint(time.Now().UnixMilli())
	raw := fmt.Sprint(d.clientID, d.clientVersion, d.packageName, d.deviceID, timestamp)
	for _, algorithm := range d.algorithms {
		raw = md5Hex(raw + algorithm)
	}
	return timestamp, "1." + raw
}

func getAction(method, rawURL string) string {
	match := regexp.MustCompile(`://[^/]+((/[^/\s?#]+)*)`).FindStringSubmatch(rawURL)
	if len(match) < 2 {
		return method + ":" + rawURL
	}
	return method + ":" + match[1]
}

func generateDeviceSign(deviceID, packageName string) string {
	signatureBase := fmt.Sprintf("%s%s%s%s", deviceID, packageName, "1", "appkey")
	sha1Hash := sha1.Sum([]byte(signatureBase))
	md5Hash := md5.Sum([]byte(hex.EncodeToString(sha1Hash[:])))
	return fmt.Sprintf("div101.%s%s", deviceID, hex.EncodeToString(md5Hash[:]))
}

func buildAndroidUserAgent(deviceID, clientID, appName, sdkVersion, clientVersion, packageName, userID string) string {
	deviceSign := generateDeviceSign(deviceID, packageName)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ANDROID-%s/%s ", appName, clientVersion))
	sb.WriteString("protocolVersion/200 accesstype/ ")
	sb.WriteString(fmt.Sprintf("clientid/%s ", clientID))
	sb.WriteString(fmt.Sprintf("clientversion/%s ", clientVersion))
	sb.WriteString("action_type/ networktype/WIFI sessionid/ ")
	sb.WriteString(fmt.Sprintf("deviceid/%s ", deviceID))
	sb.WriteString("providername/NONE ")
	sb.WriteString(fmt.Sprintf("devicesign/%s ", deviceSign))
	sb.WriteString("refresh_token/ ")
	sb.WriteString(fmt.Sprintf("sdkversion/%s ", sdkVersion))
	sb.WriteString(fmt.Sprintf("datetime/%d ", time.Now().UnixMilli()))
	sb.WriteString(fmt.Sprintf("usrno/%s ", userID))
	sb.WriteString(fmt.Sprintf("appname/android-%s ", appName))
	sb.WriteString("session_origin/ grant_type/ appid/ clientip/ ")
	sb.WriteString("devicename/Xiaomi_M2004j7ac osversion/13 platformversion/10 accessmode/ devicemodel/M2004J7AC ")
	return sb.String()
}

func md5Hex(raw string) string {
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

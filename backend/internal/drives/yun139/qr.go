package yun139

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/nianzhibai/91/backend/internal/scopedproxy"
)

const (
	qrLoginURL    = "https://user-njs.yun.139.com/user/thirdlogin"
	qrWebVersion  = "7.18.1"
	qrSessionTTL  = 5 * time.Minute
	maxQRSessions = 32
	qrHTTPTimeout = 30 * time.Second
	// Protocol keys published by the official web client (not account secrets).
	qrEnvelopeKey   = "UqEZkrjCKfa02pP6jntzFmkzOz86zHUC"
	qrCredentialKey = "qPqDw263XgFgL3u8"
)

type QRConfig struct {
	HTTPClient *http.Client
	Now        func() time.Time
}

type QRClient struct {
	client   *http.Client
	now      func() time.Time
	mu       sync.Mutex
	sessions map[string]*qrSession
}

type qrSession struct {
	mu        sync.Mutex
	deviceID  string
	expiresAt time.Time
	terminal  *QRCodeStatus
}

type QRCodeSession struct {
	SessionID      string `json:"sessionId"`
	QRCodeURL      string `json:"qrCodeUrl"`
	QRImageDataURL string `json:"qrImageDataUrl"`
	ExpiresAt      string `json:"expiresAt"`
}

type QRCodeStatus struct {
	State         string `json:"state"`
	StatusText    string `json:"statusText"`
	Authorization string `json:"authorization,omitempty"`
}

func NewQRClient(config QRConfig) *QRClient {
	client := config.HTTPClient
	if client == nil {
		client = scopedproxy.NewHTTPClient(qrHTTPTimeout)
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &QRClient{client: &copy, now: now, sessions: make(map[string]*qrSession)}
}

// The official site creates its QR URL locally. The phone registers the random
// session ID when it scans; thirdlogin then returns the approval state.
func (c *QRClient) Generate(ctx context.Context) (QRCodeSession, error) {
	if err := ctx.Err(); err != nil {
		return QRCodeSession{}, err
	}
	device := make([]byte, 16)
	if _, err := rand.Read(device); err != nil {
		return QRCodeSession{}, err
	}
	id, deviceID := rand.Text()[:16], hex.EncodeToString(device)
	qrURL := "https://yun.139.com/w/#/qrcLogin?" + url.Values{
		"sID": {id}, "dID": {deviceID}, "cType": {"9"},
	}.Encode()
	png, err := qrcode.Encode(qrURL, qrcode.Medium, 240)
	if err != nil {
		return QRCodeSession{}, fmt.Errorf("yun139 qr: encode image: %w", err)
	}
	expires := c.now().Add(qrSessionTTL)
	c.mu.Lock()
	oldestID := ""
	var oldest time.Time
	for key, session := range c.sessions {
		if !c.now().Before(session.expiresAt) {
			delete(c.sessions, key)
			continue
		}
		if oldestID == "" || session.expiresAt.Before(oldest) {
			oldestID, oldest = key, session.expiresAt
		}
	}
	if len(c.sessions) >= maxQRSessions {
		delete(c.sessions, oldestID)
	}
	c.sessions[id] = &qrSession{deviceID: deviceID, expiresAt: expires}
	c.mu.Unlock()
	return QRCodeSession{SessionID: id, QRCodeURL: qrURL, QRImageDataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), ExpiresAt: expires.Format(time.RFC3339)}, nil
}

func (c *QRClient) Poll(ctx context.Context, id string) (QRCodeStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, qrHTTPTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return QRCodeStatus{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return QRCodeStatus{}, errors.New("sessionId is required")
	}
	c.mu.Lock()
	session := c.sessions[id]
	c.mu.Unlock()
	if session == nil {
		return qrStatus("expired"), nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !c.now().Before(session.expiresAt) {
		c.mu.Lock()
		delete(c.sessions, id)
		c.mu.Unlock()
		return qrStatus("expired"), nil
	}
	if session.terminal != nil {
		return *session.terminal, nil
	}
	status, err := c.query(ctx, id, session.deviceID)
	if err != nil {
		return QRCodeStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return QRCodeStatus{}, err
	}
	if !c.now().Before(session.expiresAt) {
		return qrStatus("expired"), nil
	}
	if status.State != "waiting" && status.State != "scanned" {
		session.terminal = &status
	}
	return status, nil
}

type qrLoginData struct {
	Account   string `json:"account"`
	Token     string `json:"token"`
	AuthToken string `json:"authToken"`
	Result    struct {
		Code string `json:"resultCode"`
	} `json:"result"`
}

func (c *QRClient) query(ctx context.Context, id, deviceID string) (QRCodeStatus, error) {
	secinfo := sha1.Sum([]byte("fetion.com.cn:" + id))
	body, err := json.Marshal(map[string]any{
		"msisdn": "", "random": "", "dycpwd": id, "cpid": 292, "clienttype": 670,
		"version": "mCloud_4.3.0_536", "pintype": 21,
		"secinfo":   strings.ToUpper(hex.EncodeToString(secinfo[:])),
		"loginMode": "0", "extInfo": map[string]any{},
	})
	if err != nil {
		return QRCodeStatus{}, err
	}
	payload, err := encryptQRRequest(body)
	if err != nil {
		return QRCodeStatus{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qrLoginURL, strings.NewReader(payload))
	if err != nil {
		return QRCodeStatus{}, err
	}
	req.Header = signedHeaders(body, deviceID, qrWebVersion, c.now())
	req.Header.Set("hcy-cool-flag", "1")
	data, err := doRequest(c.client, req)
	if err != nil {
		return QRCodeStatus{}, err
	}
	data, err = decryptQREnvelope(data)
	if err != nil {
		return QRCodeStatus{}, err
	}
	var result struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return QRCodeStatus{}, errors.New("yun139 qr: invalid login response")
	}
	var login qrLoginData
	if len(result.Data) > 0 && !bytes.Equal(result.Data, []byte("null")) {
		plain := result.Data
		if plain[0] == '"' {
			var encoded string
			if err := json.Unmarshal(plain, &encoded); err != nil {
				return QRCodeStatus{}, errors.New("yun139 qr: invalid credential response")
			}
			ciphertext, err := hex.DecodeString(encoded)
			if err != nil {
				return QRCodeStatus{}, errors.New("yun139 qr: invalid credential encoding")
			}
			plain, err = decryptQRCredential(ciphertext)
			if err != nil {
				return QRCodeStatus{}, err
			}
		}
		if err := json.Unmarshal(plain, &login); err != nil {
			return QRCodeStatus{}, errors.New("yun139 qr: invalid credential data")
		}
	}
	code := strings.Trim(string(result.Code), `"`)
	if login.Result.Code != "" {
		code = login.Result.Code
	}
	switch code {
	case "200059541":
		return qrStatus("waiting"), nil
	case "200059548":
		return qrStatus("scanned"), nil
	case "200059542":
		return qrStatus("expired"), nil
	case "200059549":
		return qrStatus("canceled"), nil
	case "200059543", "200059545", "200059546", "200059547":
		return qrStatus("error"), nil
	case "0", "0000":
		token := login.AuthToken
		if token == "" {
			token = login.Token
		}
		authorization := base64.StdEncoding.EncodeToString([]byte("pc:" + login.Account + ":" + token))
		_, _, _, expiry, err := parseAuthorization(authorization)
		if err != nil || !expiry.After(c.now()) {
			return QRCodeStatus{}, errors.New("yun139 qr: login succeeded without a valid Authorization")
		}
		status := qrStatus("success")
		status.Authorization = authorization
		return status, nil
	default:
		return QRCodeStatus{}, errors.New("移动网盘扫码请求失败，请稍后重试")
	}
}

func qrStatus(state string) QRCodeStatus {
	text := map[string]string{
		"waiting": "请使用中国移动云盘 App 或微信扫码",
		"scanned": "已扫码，请在手机上确认登录", "success": "登录成功",
		"expired": "二维码已过期，请重新生成", "canceled": "已取消登录，请重新生成二维码",
		"error": "扫码登录失败，请重新生成二维码",
	}
	return QRCodeStatus{State: state, StatusText: text[state]}
}

func encryptQRRequest(plain []byte) (string, error) {
	block, err := aes.NewCipher([]byte(qrEnvelopeKey))
	if err != nil {
		return "", err
	}
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(padding)}, padding)...)
	encoded := make([]byte, aes.BlockSize+len(padded))
	if _, err := io.ReadFull(rand.Reader, encoded[:aes.BlockSize]); err != nil {
		return "", err
	}
	cipher.NewCBCEncrypter(block, encoded[:aes.BlockSize]).CryptBlocks(encoded[aes.BlockSize:], padded)
	return base64.StdEncoding.EncodeToString(encoded), nil
}

func decryptQREnvelope(data []byte) ([]byte, error) {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '{' {
		return data, nil
	}
	if len(data) > 0 && data[0] == '"' {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return nil, errors.New("yun139 qr: invalid encrypted response")
		}
		data = []byte(encoded)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil || len(decoded) < 2*aes.BlockSize || len(decoded)%aes.BlockSize != 0 {
		return nil, errors.New("yun139 qr: invalid encrypted response")
	}
	block, err := aes.NewCipher([]byte(qrEnvelopeKey))
	if err != nil {
		return nil, err
	}
	cipher.NewCBCDecrypter(block, decoded[:aes.BlockSize]).CryptBlocks(decoded[aes.BlockSize:], decoded[aes.BlockSize:])
	return unpadQRResponse(decoded[aes.BlockSize:])
}

func decryptQRCredential(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("yun139 qr: invalid encrypted credential")
	}
	block, err := aes.NewCipher([]byte(qrCredentialKey))
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(data))
	for start := 0; start < len(data); start += aes.BlockSize {
		block.Decrypt(plain[start:start+aes.BlockSize], data[start:start+aes.BlockSize])
	}
	return unpadQRResponse(plain)
}

func unpadQRResponse(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("yun139 qr: empty encrypted response")
	}
	padding := int(data[len(data)-1])
	if padding < 1 || padding > aes.BlockSize || padding > len(data) || !bytes.Equal(data[len(data)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		return nil, errors.New("yun139 qr: invalid response padding")
	}
	return data[:len(data)-padding], nil
}

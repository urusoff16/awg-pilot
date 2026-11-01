// Package keyapi talks to the subscription gateway behind a vpn:// key
// (the "client-api/v1" protocol).
package keyapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"awgpilot/internal/netx"
)

const (
	DefaultDownloadPath = "client-api/v1/download-awg-key"
	maxBody             = 4 << 20
	// SupportedAWGVersion is sent as X-Supported-Awg-Version. Without it the gateway looks for
	// legacy servers and answers 406 available_wg_server_not_found; "2" is rejected as invalid.
	SupportedAWGVersion = "2.0"
)

// Client is safe for concurrent use.
type Client struct {
	BaseURL   string // e.g. https://api.stkv.space
	DeviceID  string // sent as X-Device-Id; reuse the official app's HWID so no extra device slot is used
	UserAgent string
	HTTP      *http.Client
}

func New(baseURL, deviceID, userAgent string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		DeviceID:  deviceID,
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: 30 * time.Second, Transport: netx.Transport()},
	}
}

// APIError is the gateway's error object; Code values seen in the app include
// "change_server_limit_exceeded", "no_active_key" and "key_limit_exceeded".
type APIError struct {
	Code             string `json:"code"`
	LocalizedMessage string `json:"localized_message"`
	Status           int    `json:"-"`
}

// Russian fallbacks for gateway error codes seen in the official client.
var codeMessages = map[string]string{
	"change_server_limit_exceeded": "исчерпан лимит смены сервера, попробуйте позже",
	"no_active_key":                "у подписки нет активного ключа",
	"key_limit_exceeded":           "достигнут лимит устройств подписки: освободите место в личном кабинете провайдера (Telegram-бот) или привяжите ключ к этому устройству",
}

func (e *APIError) Error() string {
	if m, ok := codeMessages[e.Code]; ok {
		return m
	}
	if e.LocalizedMessage != "" {
		if !hasCyrillic(e.LocalizedMessage) && e.Code != "" {
			return e.LocalizedMessage + " (" + e.Code + ")"
		}
		return e.LocalizedMessage // the gateway localizes some of its messages
	}
	if e.Code != "" {
		return "ошибка сервера: " + e.Code
	}
	switch {
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return fmt.Sprintf("сервер отказал в доступе (HTTP %d) — проверьте ключ", e.Status)
	case e.Status == http.StatusNotFound:
		return "сервер не нашёл ключ или адрес (HTTP 404)"
	case e.Status == http.StatusTooManyRequests:
		return "слишком много запросов, подождите немного (HTTP 429)"
	case e.Status >= 500:
		return fmt.Sprintf("сервер провайдера временно недоступен (HTTP %d)", e.Status)
	}
	return fmt.Sprintf("неожиданный ответ сервера (HTTP %d)", e.Status)
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if r >= 0x400 && r <= 0x4ff {
			return true
		}
	}
	return false
}

type User struct {
	Email string `json:"email"`
}

// Request is the subscription ("request") as reported by get-request.
type Request struct {
	PublicRequestID       string   `json:"public_request_id"`
	SimplifiedStatus      string   `json:"simplified_status"`
	PaidUntil             string   `json:"paid_until"`
	PaymentLink           string   `json:"payment_link"`
	Alias                 string   `json:"alias,omitempty"`
	IsBrandNew            bool     `json:"is_brand_new"`
	NumberOfAvailableKeys float64  `json:"number_of_available_keys"`
	KeyLimit              float64  `json:"key_limit"`
	User                  User     `json:"user"`
	AutoRenewal           bool     `json:"auto_renewal,omitempty"`
	PriceRub              *float64 `json:"price_rub,omitempty"`
	Period                *float64 `json:"period,omitempty"`
	PeriodName            string   `json:"period_name,omitempty"`
}

type Country struct {
	CountryLabel   string `json:"country_label"`
	ISOCountryCode string `json:"iso_country_code"`
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *APIError       `json:"error"`
}

func (c *Client) endpoint(path string, query url.Values) string {
	u := c.BaseURL + "/" + strings.TrimLeft(path, "/")
	if len(query) == 0 {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + query.Encode()
}

func (c *Client) do(ctx context.Context, method, rawURL string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if c.DeviceID != "" {
		req.Header.Set("X-Device-Id", c.DeviceID)
	}
	req.Header.Set("X-Supported-Awg-Version", SupportedAWGVersion)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			return nil, errors.New("сервер провайдера не ответил вовремя — проверьте интернет")
		}
		// *url.Error repeats the full URL, and get-request carries the key's ID in it: keep it
		// out of the journal.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("нет связи с сервером провайдера: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("ответ сервера оборвался: %w", err)
	}
	// download-awg-key answers 200 text/plain with the wg-quick config itself.
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 && isWgQuick(data) {
		return data, nil
	}
	var env envelope
	jsonErr := json.Unmarshal(data, &env)
	if env.Error != nil && (env.Error.Code != "" || env.Error.LocalizedMessage != "") {
		env.Error.Status = resp.StatusCode
		return nil, env.Error
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Status: resp.StatusCode}
	}
	if jsonErr != nil {
		if json.Valid(data) { // e.g. a bare array
			return data, nil
		}
		return nil, errors.New("сервер вернул ответ в непонятном формате")
	}
	if len(env.Data) > 0 && string(env.Data) != "null" {
		return env.Data, nil
	}
	return data, nil
}

// GetRequest returns the subscription status for publicRequestID.
func (c *Client) GetRequest(ctx context.Context, publicRequestID string) (*Request, error) {
	data, err := c.do(ctx, http.MethodGet, c.endpoint("client-api/v1/get-request", url.Values{"public_request_id": {publicRequestID}}), nil)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Request *Request `json:"request"`
	}
	if json.Unmarshal(data, &wrapped) == nil && wrapped.Request != nil {
		return wrapped.Request, nil
	}
	var r Request
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errors.New("не удалось разобрать данные подписки")
	}
	return &r, nil
}

// GetCountries returns the server locations offered to desktop clients. Some brands of the
// gateway do not offer a choice (the endpoint answers 404); that yields an empty list, not an error.
func (c *Client) GetCountries(ctx context.Context) ([]Country, error) {
	data, err := c.do(ctx, http.MethodGet, c.endpoint("client-api/v1/countries/desktop/country-list.json", nil), nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Code == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Country
	if err := json.Unmarshal(data, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Countries []Country `json:"countries"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, errors.New("не удалось разобрать список стран")
	}
	return wrapped.Countries, nil
}

// DownloadOptions select which configuration the gateway issues.
type DownloadOptions struct {
	RelativePath   string // from remote config connections[].relative_path; empty = DefaultDownloadPath
	ISOCountryCode string // empty = server default
	KeyType        string // connections[].key_type ("" = smart mode)
	// ForceUpdateDevice re-binds the key to this device. It can unbind another device,
	// so callers must only set it after explicit user confirmation.
	ForceUpdateDevice bool
}

// DownloadAWGKey returns an AmneziaWG wg-quick style configuration.
func (c *Client) DownloadAWGKey(ctx context.Context, publicRequestID string, o DownloadOptions) (string, error) {
	path := o.RelativePath
	if path == "" {
		path = DefaultDownloadPath
	}
	q := url.Values{"public_request_id": {publicRequestID}}
	if o.ISOCountryCode != "" {
		q.Set("iso_country_code", o.ISOCountryCode)
	}
	if o.KeyType != "" {
		q.Set("key_type", o.KeyType)
	}
	if o.ForceUpdateDevice {
		q.Set("force_update_device", "true")
	}
	data, err := c.do(ctx, http.MethodGet, c.endpoint(path, q), nil)
	if err != nil {
		return "", err
	}
	if isWgQuick(data) {
		return string(data), nil
	}
	var cfg struct {
		RawConfig string `json:"raw_config"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", errors.New("не удалось разобрать ответ с конфигом")
	}
	if strings.TrimSpace(cfg.RawConfig) == "" {
		return "", errors.New("сервер прислал пустой конфиг")
	}
	return cfg.RawConfig, nil
}

func isWgQuick(b []byte) bool {
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	return bytes.HasPrefix(bytes.TrimSpace(b), []byte("[Interface]"))
}

// CheckChangeServer reports whether a server change is currently allowed.
func (c *Client) CheckChangeServer(ctx context.Context, publicRequestID, keyType, iso string) (bool, error) {
	q := url.Values{"public_request_id": {publicRequestID}}
	q.Set("key_type", keyType)
	if iso != "" {
		q.Set("iso_country_code", iso)
	}
	data, err := c.do(ctx, http.MethodGet, c.endpoint("client-api/v1/check-change-server", q), nil)
	if err != nil {
		return false, err
	}
	var r struct {
		Available bool `json:"is_change_server_available"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return false, errors.New("не удалось разобрать ответ о смене сервера")
	}
	return r.Available, nil
}

// ChangeServer asks the gateway to move this key to another server (rate limited server side).
func (c *Client) ChangeServer(ctx context.Context, publicRequestID, keyType, iso string) error {
	body := map[string]string{"public_request_id": publicRequestID, "key_type": keyType}
	if iso != "" {
		body["iso_country_code"] = iso
	}
	data, err := c.do(ctx, http.MethodPost, c.endpoint("client-api/v1/change-server", nil), body)
	if err != nil {
		return err
	}
	var r struct {
		Success *bool `json:"success"`
	}
	if json.Unmarshal(data, &r) == nil && r.Success != nil && !*r.Success {
		return errors.New("сервер не принял запрос на смену")
	}
	return nil
}

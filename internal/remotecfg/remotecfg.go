// Package remotecfg fetches the provider's public desktop configuration
// (API base URL, connection types, recommended DNS).
package remotecfg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"awgpilot/internal/netx"
)

const (
	GatewayConfigURL = "https://storage.yandexcloud.net/vpnn-web-configs/sotka/desktop/config.json"
	GatewayAPI       = "https://api.stkv.space" // keys for this gateway use GatewayConfigURL's connection types
)

type Connection struct {
	Title          string            `json:"title"`
	LocalizedTitle map[string]string `json:"localized_title"`
	RelativePath   string            `json:"relative_path"`
	KeyType        string            `json:"key_type"`
}

type Config struct {
	APIBaseURL            string       `json:"api_base_url"`
	DNS1                  string       `json:"dns1"`
	DNS2                  string       `json:"dns2"`
	SupportLink           string       `json:"support_link"`
	ProfileBotLink        string       `json:"profile_bot_link"`
	IsChangeServerEnabled bool         `json:"is_change_server_enabled"`
	Connections           []Connection `json:"connections"`
}

// Fetch downloads and parses the remote configuration.
func Fetch(ctx context.Context, url string) (*Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := netx.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("настройки провайдера недоступны (HTTP %d)", resp.StatusCode)
	}
	var c Config
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Connection returns the connection for keyType, or the first one.
func (c *Config) Connection(keyType string) *Connection {
	for i := range c.Connections {
		if c.Connections[i].KeyType == keyType {
			return &c.Connections[i]
		}
	}
	if len(c.Connections) > 0 {
		return &c.Connections[0]
	}
	return nil
}

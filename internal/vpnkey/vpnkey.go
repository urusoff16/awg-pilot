// Package vpnkey decodes Amnezia-style "vpn://" access keys.
//
// Layout: base64url( 4-byte big-endian length || zlib(JSON) ).
package vpnkey

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const scheme = "vpn://"

// maxDecoded bounds decompression so a hostile key cannot exhaust memory.
const maxDecoded = 1 << 20

// Key is the gateway form of an access key: an API endpoint plus a per-subscription identifier.
type Key struct {
	APIEndpoint   string `json:"api_endpoint"`
	APIKey        string `json:"api_key"`
	ConfigVersion int    `json:"config_version"`
	Description   string `json:"description"`
	Protocol      string `json:"protocol"`
}

// Decode parses a vpn:// key. Surrounding whitespace is ignored.
func Decode(s string) (*Key, error) {
	plain, err := decodeJSON(s)
	if err != nil {
		return nil, err
	}
	var k Key
	if err := json.Unmarshal(plain, &k); err != nil {
		return nil, errors.New("ключ повреждён: внутри не JSON")
	}
	if k.APIEndpoint == "" || k.APIKey == "" {
		return nil, errors.New("в ключе нет адреса API или идентификатора — поддерживаются только ключи с сервером выдачи конфигов")
	}
	if !strings.HasPrefix(k.APIEndpoint, "https://") {
		return nil, errors.New("адрес API в ключе должен использовать https")
	}
	k.APIEndpoint = strings.TrimRight(k.APIEndpoint, "/")
	return &k, nil
}

// Native is a key shared from the AmneziaVPN app for a self-hosted server: it carries ready
// connection settings instead of a provider API.
type Native struct {
	Name   string // the key's description, or the server address
	Host   string
	Config string   // AmneziaWG wg-quick config; empty when the key has no AmneziaWG
	Others []string // other protocols in the key (amnezia-xray, ...)
}

// DecodeNative parses an AmneziaVPN share key ("vpn://" with "containers").
func DecodeNative(s string) (*Native, error) {
	plain, err := decodeJSON(s)
	if err != nil {
		return nil, err
	}
	var k struct {
		Description string `json:"description"`
		HostName    string `json:"hostName"`
		DNS1        string `json:"dns1"`
		DNS2        string `json:"dns2"`
		Containers  []struct {
			Container string `json:"container"`
			AWG       *struct {
				LastConfig string `json:"last_config"`
			} `json:"awg"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(plain, &k); err != nil || len(k.Containers) == 0 {
		return nil, errors.New("это не ключ AmneziaVPN")
	}
	n := &Native{Name: strings.TrimSpace(k.Description), Host: k.HostName}
	if n.Name == "" || !utf8.ValidString(n.Name) {
		n.Name = k.HostName
	}
	for _, c := range k.Containers {
		if c.AWG == nil || c.AWG.LastConfig == "" || n.Config != "" {
			n.Others = append(n.Others, c.Container)
			continue
		}
		var last struct {
			Config string `json:"config"`
		}
		if json.Unmarshal([]byte(c.AWG.LastConfig), &last) != nil || last.Config == "" {
			continue
		}
		dns2 := k.DNS2
		if dns2 == "" {
			dns2 = k.DNS1
		}
		n.Config = strings.NewReplacer("$PRIMARY_DNS", k.DNS1, "$SECONDARY_DNS", dns2).Replace(last.Config)
	}
	return n, nil
}

func decodeJSON(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(s), scheme) {
		return nil, errors.New("ключ должен начинаться с vpn://")
	}
	body := strings.TrimRight(s[len(scheme):], "=")
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errors.New("ключ повреждён: недопустимые символы (скопируйте его целиком)")
	}
	if len(raw) < 6 {
		return nil, errors.New("ключ слишком короткий — скопируйте его целиком")
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw[4:]))
	if err != nil {
		return nil, errors.New("ключ повреждён: не удалось распаковать данные")
	}
	defer zr.Close()
	plain, err := io.ReadAll(io.LimitReader(zr, maxDecoded+1))
	if err != nil {
		return nil, errors.New("ключ повреждён: данные обрываются (скопируйте его целиком)")
	}
	if len(plain) > maxDecoded {
		return nil, errors.New("ключ слишком большой")
	}
	return plain, nil
}

// Encode produces a vpn:// key for k; used by tests and for exporting.
func Encode(k *Key) (string, error) {
	plain, err := json.Marshal(k)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	n := len(plain)
	buf.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return scheme + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// Masked returns the API key with all but the first and last four characters hidden.
func (k *Key) Masked() string {
	if len(k.APIKey) <= 8 {
		return "***"
	}
	return k.APIKey[:4] + "…" + k.APIKey[len(k.APIKey)-4:]
}

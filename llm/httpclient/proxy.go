package httpclient

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

type ProxyType string

const (
	ProxyTypeDisabled    ProxyType = "disabled"
	ProxyTypeEnvironment ProxyType = "environment"
	ProxyTypeURL         ProxyType = "url"
)

type ProxyConfig struct {
	Type                   ProxyType `json:"type"`
	URL                    string    `json:"url,omitempty"`
	Username               string    `json:"username,omitempty"`
	Password               string    `json:"password,omitempty"`
	DisableConnectionReuse bool      `json:"disableConnectionReuse,omitempty"`
}

func (c *ProxyConfig) parsedURL() (*url.URL, error) {
	if c == nil || strings.TrimSpace(c.URL) == "" {
		return nil, fmt.Errorf("proxy URL is required when type is 'url'")
	}
	parsed, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	if c.Username != "" {
		parsed.User = url.UserPassword(c.Username, c.Password)
	}
	return parsed, nil
}

func isSOCKSScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "socks5", "socks5h", "socks":
		return true
	default:
		return false
	}
}

func socksDialContext(proxyURL *url.URL) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	var auth *proxy.Auth
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
	if err != nil {
		return nil, err
	}
	if cd, ok := dialer.(proxy.ContextDialer); ok {
		return cd.DialContext, nil
	}
	return func(_ context.Context, network, addr string) (net.Conn, error) {
		return dialer.Dial(network, addr)
	}, nil
}

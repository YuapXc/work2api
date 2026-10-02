package app

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
)

// DNS 抖动豁免（buddy-proxy #62）：解析失败必须被识别，机器级故障不该打账号冷却。
func TestIsLocalDNSFailure(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "api.example.com", IsNotFound: true}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare dns error", dnsErr, true},
		{"wrapped by url.Error", &url.Error{Op: "Post", URL: "https://x", Err: dnsErr}, true},
		{"wrapped deep", fmt.Errorf("forward failed: %w", fmt.Errorf("dial: %w", dnsErr)), true},
		{"timeout dns", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, true},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
		{"generic error", errors.New("EOF"), false},
		{"timeout net error", &net.DNSError{Err: "timeout", IsTimeout: false, Name: "x"}, true},
	}
	for _, tc := range cases {
		if got := isLocalDNSFailure(tc.err); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

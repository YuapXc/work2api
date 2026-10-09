package app

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
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

func TestLocalNetworkClassificationDoesNotGuess(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}}, true},
		{syscall.EHOSTUNREACH, true}, {syscall.EADDRNOTAVAIL, true},
		{syscall.ECONNREFUSED, false}, {syscall.ECONNRESET, false}, {syscall.ETIMEDOUT, false},
		{errors.New("network is unreachable"), false}, {errors.New("proxyconnect tcp: 502"), false},
	} {
		if got := isLocalNetworkFailure(tc.err); got != tc.want {
			t.Fatalf("%v: got %v want %v", tc.err, got, tc.want)
		}
	}
}

package controller

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIPIgnoresForgedXFFFromUntrustedRemote(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/api/session", nil)
	req.RemoteAddr = "198.51.100.20:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if got := clientIP(req, trusted); got != "198.51.100.20" {
		t.Fatalf("clientIP trusted forged XFF: got %q", got)
	}
}

func TestClientIPWalksTrustedProxyChainFromRight(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/api/session", nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-For", "192.0.2.66, 198.51.100.7, 10.0.0.1")
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if got := clientIP(req, trusted); got != "198.51.100.7" {
		t.Fatalf("clientIP = %q, want nearest untrusted hop", got)
	}
}

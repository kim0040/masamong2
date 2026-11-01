package web

import (
	"net"
	"net/http"
	"strings"
)

// LoopbackHost rejects DNS-rebinding hosts before a console handles a request.
func LoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SameOrigin checks browser origin metadata for console writes.
func SameOrigin(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "cross-site", "same-site":
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return o == scheme+"://"+r.Host
	}
	return true
}

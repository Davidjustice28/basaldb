package api

import (
	"fmt"
	"net"
	"strings"
)

// ValidateListen reports whether basald may serve on addr. A non-loopback
// address with no bearer token is refused unless the operator passes
// insecure. /v1/health stays open either way; this only decides if the
// process is allowed to bind.
func ValidateListen(addr, token string, insecure bool) error {
	if Loopback(addr) || token != "" || insecure {
		return nil
	}
	return fmt.Errorf("refusing to listen on %s without a token; set the token env var or pass -insecure-no-auth", addr)
}

// Loopback reports whether addr is only reachable from this machine.
// An empty host (":8080") and the unspecified addresses are not loopback.
func Loopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

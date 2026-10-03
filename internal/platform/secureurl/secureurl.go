// Package secureurl checks the URLs Jupiter sends card data and money instructions to:
// absolute, over HTTPS, or over plain HTTP only to this machine, where simulators run.
package secureurl

import (
	"fmt"
	"net"
	"net/url"
)

// Check refuses a URL that is not absolute and HTTPS, or HTTP to this machine.
func Check(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("%q is not an absolute URL without credentials", raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && Loopback(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("%q must be https, or http to this machine", raw)
}

// Loopback says a host is this machine.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

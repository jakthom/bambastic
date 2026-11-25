package lanhttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

var forbiddenIPv6 = map[string]bool{"fd00:ec2::254": true, "fd00:42::42": true, "fd00:c1::a9fe:a9fe": true, "fd20:ce::254": true, "fd00:a9fe:a9fe::1": true, "fd00:ec2::23": true}

// The configured service can live on localhost or a private LAN. Resolve on each new connection and refuse link-local service addresses.
var Client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 4, DialContext: DialContext}}

func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if e != nil {
		return nil, e
	}
	var last error
	for _, ip := range ips {
		ip = ip.Unmap()
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.String() == "100.100.100.200" || forbiddenIPv6[ip.String()] {
			return nil, errors.New("disallowed service address")
		}
	}
	for _, ip := range ips {
		d := net.Dialer{Timeout: 3 * time.Second}
		c, e := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return c, nil
		}
		last = e
	}
	if last == nil {
		last = errors.New("host has no addresses")
	}
	return nil, last
}

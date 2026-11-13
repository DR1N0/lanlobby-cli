package room

import (
	"net"
	"os"
	"strings"
)

func hostname() (string, error) { return os.Hostname() }

// DefaultName is the fallback member/node name: the short host name.
func DefaultName() string {
	h, err := hostname()
	if err != nil || h == "" {
		return "guest"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	if n := SanitizeName(h); n != "" {
		return n
	}
	return "guest"
}

// LocalIP returns a plausible LAN address to advertise, preferring private
// IPv4 addresses over other routable ones.
func LocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	fallback := ""
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			if isPrivate(ip) {
				return ip.String()
			}
			if fallback == "" {
				fallback = ip.String()
			}
		}
	}
	return fallback
}

func isPrivate(ip net.IP) bool {
	if ip[0] == 10 {
		return true
	}
	if ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31 {
		return true
	}
	return ip[0] == 192 && ip[1] == 168
}

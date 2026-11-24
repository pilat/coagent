package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

func (c APIConfig) validateAPIListen() error {
	if c.Listen == "" {
		return nil
	}

	if !loopbackAddress(c.Listen) {
		return fmt.Errorf("api.listen %q must be a loopback IP address and port, such as 127.0.0.1:7878", c.Listen)
	}

	if len(c.Clients) == 0 {
		return errors.New("api.listen requires at least one api client")
	}

	return nil
}

func (c APIConfig) validateAPIOrigins() error {
	if len(c.Origins) > 0 && c.Listen == "" {
		return errors.New("api.origins requires api.listen")
	}

	seen := make(map[string]bool)

	for _, origin := range c.Origins {
		if strings.Contains(origin, "*") {
			return fmt.Errorf("api origin %q must be an exact origin; wildcards are not supported", origin)
		}

		if !validAPIOrigin(origin) {
			return fmt.Errorf("api origin %q must be scheme://host[:port] with scheme http or https, "+
				"in lowercase, without a default port, path, query or credentials", origin)
		}

		if seen[origin] {
			return fmt.Errorf("api origin %q is listed twice", origin)
		}

		seen[origin] = true
	}

	return nil
}

func loopbackAddress(listen string) bool {
	address, err := netip.ParseAddrPort(listen)
	if err != nil {
		return false
	}

	if !address.Addr().IsLoopback() {
		return false
	}

	if address.Addr().Zone() != "" {
		return false
	}

	if address.Addr().Is4In6() {
		return false
	}

	if address.Port() == 0 {
		return false
	}

	return address.String() == listen
}

func validAPIOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}

	if u.Hostname() == "" {
		return false
	}

	if origin != strings.ToLower(origin) {
		return false
	}

	if origin != u.Scheme+"://"+u.Host {
		return false
	}

	return validAPIOriginPort(u)
}

func validAPIOriginPort(u *url.URL) bool {
	if strings.HasSuffix(u.Host, ":") {
		return false
	}

	if u.Port() == "" {
		return true
	}

	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
		return false
	}

	if u.Scheme == "http" {
		return port != 80
	}

	return port != 443
}

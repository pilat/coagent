package config

import (
	"fmt"
	"net/url"
	"strings"
)

func validateTelegramAPIURL(m *ManagerEntry) error {
	if m.APIURL == "" {
		return nil
	}

	parsed, err := url.Parse(m.APIURL)

	validScheme := err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
	if !validScheme || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf(
			"manager %q (driver: telegram) requires \"api_url\" to be an HTTP(S) base URL without credentials, query, or fragment",
			m.ID,
		)
	}

	m.APIURL = strings.TrimRight(m.APIURL, "/")

	return nil
}

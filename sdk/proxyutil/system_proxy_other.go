//go:build !darwin

package proxyutil

import "net/url"

func proxyFromOperatingSystem(_ *url.URL) (*url.URL, error) {
	return nil, nil
}

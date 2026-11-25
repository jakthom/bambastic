//go:build dev

package main

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

func browserHandler() (http.Handler, error) {
	targetURL := os.Getenv("BAMBASTIC_VITE_URL")
	if targetURL == "" {
		return site(os.DirFS("frontend/dist")), nil
	}
	target, err := url.Parse(targetURL)
	if err != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" {
		return nil, fmt.Errorf("development frontend requires a loopback BAMBASTIC_VITE_URL")
	}
	return httputil.NewSingleHostReverseProxy(target), nil
}

const developmentBuild = true

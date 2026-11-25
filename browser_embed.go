//go:build !dev

package main

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed frontend/dist
var bundle embed.FS

func browserHandler() (http.Handler, error) {
	assets, err := fs.Sub(bundle, "frontend/dist")
	if err != nil {
		return nil, err
	}
	return site(assets), nil
}

const developmentBuild = false

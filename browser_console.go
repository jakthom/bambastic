//go:build console && !dev

package main

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/dist
var consoleFiles embed.FS

func browserHandler() (http.Handler, error) {
	files, err := fs.Sub(consoleFiles, "web/dist")
	if err != nil {
		return nil, err
	}
	return site(files), nil
}

const developmentBuild = false

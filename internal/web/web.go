package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/*
var assets embed.FS

func Handler() http.Handler {
	f, e := fs.Sub(assets, "static")
	if e != nil {
		panic(e)
	}
	return http.FileServer(http.FS(f))
}

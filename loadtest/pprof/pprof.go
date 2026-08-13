// Package main is not built in place. build.sh copies this file into the
// server's cmd/server directory before compiling the load-test image, so
// profiles can be taken from a running container without the shipped binary
// carrying a pprof listener.
//
//	go tool pprof http://localhost:6060/debug/pprof/heap
//	curl -s 'localhost:6060/debug/pprof/goroutine?debug=1' | head -1
//	curl -s 'localhost:6060/debug/pprof/heap?debug=1' | grep '^# Stack'
package main

import (
	"net/http"
	_ "net/http/pprof"
)

func init() {
	go func() { _ = http.ListenAndServe("0.0.0.0:6060", nil) }()
}

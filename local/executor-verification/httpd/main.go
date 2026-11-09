// Tiny disposable HTTP service for the local integration containers.
package main

import (
	"fmt"
	"net/http"
	"time"
)

func main() {
	server := &http.Server{
		Addr: ":8080", ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintln(w, "ok")
		}),
	}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}

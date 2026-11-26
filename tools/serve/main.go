// Command serve is a range-capable static file server used to exercise ODM
// end to end. http.ServeFile already answers Range requests with 206, which is
// exactly the shape the segmented downloader needs.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
)

func main() {
	dir := flag.String("dir", ".", "directory to serve")
	port := flag.Int("port", 0, "port (0 picks a free one)")
	flag.Parse()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	// The first line on stdout is the address, so a caller can wait for it.
	fmt.Printf("http://127.0.0.1:%d\n", addr.Port)

	h := http.FileServer(http.Dir(*dir))
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		h.ServeHTTP(w, r)
	})}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

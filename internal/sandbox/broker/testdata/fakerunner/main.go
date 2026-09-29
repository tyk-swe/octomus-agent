// Command fakerunner stands in for codex and opencode inside the broker's Docker test image.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	name := filepath.Base(os.Args[0])
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Printf("%s-fake 1.0.0\n", name)
		return
	}
	switch {
	case name == "codex" && len(args) > 0 && args[0] == "app-server":
		fmt.Fprintln(os.Stderr, "codex diagnostic")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1<<20), 32<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "exit" {
				os.Exit(4)
			}
			fmt.Printf("echo:%d:%s\n", len(line), strings.ToUpper(line[:min(len(line), 16)]))
		}
	case name == "opencode" && len(args) > 0 && args[0] == "serve":
		if os.Getenv("OPENCODE_FAKE_FAIL") == "1" {
			fmt.Fprintln(os.Stderr, "fake startup failure")
			os.Exit(1)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(1)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
			user, password, _ := r.BasicAuth()
			fmt.Fprintf(w, `{"healthy":true,"version":"fake","user":%q,"password":%q,"policy":%q}`, user, password, os.Getenv("OPENCODE_SERVER_PASSWORD"))
		})
		mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			fmt.Fprintf(w, "%d", n)
		})
		mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := range 3 {
				fmt.Fprintf(w, "data: %d\n\n", i)
				w.(http.Flusher).Flush()
				time.Sleep(50 * time.Millisecond)
			}
		})
		fmt.Printf("opencode server listening on http://%s\n", listener.Addr())
		_ = http.Serve(listener, mux)
	default:
		fmt.Fprintf(os.Stderr, "fakerunner: unexpected %s %v\n", name, args)
		os.Exit(2)
	}
}

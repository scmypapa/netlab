//go:build ignore

package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil || cwd != "/work" || os.Getuid() != 1000 || os.Getgid() != 1000 || len(os.Args) != 2 || os.Args[1] != "image-argument" || os.Getenv("IMAGE_DEFAULT") != "present" || os.Getenv("OVERRIDE_ME") != "asset" {
		log.Fatalf("image process configuration changed: cwd=%q uid=%d gid=%d args=%v error=%v", cwd, os.Getuid(), os.Getgid(), os.Args, err)
	}
	for path, expected := range map[string]string{"/etc/hosts": "127.0.0.1 localhost", "/etc/resolv.conf": "nameserver 192.0.2.53"} {
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), expected) {
			log.Fatalf("read network configuration %s: %q %v", path, data, err)
		}
	}
	if configured := os.Getenv("NETLAB_TEST_STOP_DELAY"); configured != "" {
		delay, err := time.ParseDuration(configured)
		if err != nil {
			log.Fatal(err)
		}
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGTERM)
		go func() { <-stopping; time.Sleep(delay); os.Exit(0) }()
	}
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ready\n")) })
	log.Fatal(http.ListenAndServe(":502", nil))
}

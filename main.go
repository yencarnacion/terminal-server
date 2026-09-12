package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed quotes.txt
var bundledQuotes string

func main() {
	listen := flag.String("listen", ":8177", "HTTP bind address")
	base := flag.String("base-url", "http://10.17.17.90:8177", "URL reachable by the device")
	quotes := flag.String("quotes", "", "optional fortune-format file; defaults to bundled quotes")
	data := flag.String("data-dir", "data", "persistent device key directory")
	refresh := flag.Int("refresh", 600, "device refresh interval in seconds")
	screen := flag.String("screen", "cowsay", "screen generator")
	flag.Parse()
	u, err := url.Parse(*base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		log.Fatal("base-url must be an absolute HTTP(S) origin")
	}
	if *refresh < 60 {
		log.Fatal("refresh must be at least 60 seconds")
	}
	content := []byte(bundledQuotes)
	if *quotes != "" {
		content, err = os.ReadFile(*quotes)
		if err != nil {
			log.Fatal(err)
		}
	}
	fortunes, err := parseQuotes(string(content))
	if err != nil {
		log.Fatal(err)
	}
	key, err := loadKey(*data)
	if err != nil {
		log.Fatal(err)
	}
	app, err := newApp(strings.TrimRight(*base, "/"), *refresh, *screen, fortunes, key)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: app.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("TRMNL X: %d quotes, screen=%s, listen=%s, preview=%s/preview", len(fortunes), *screen, *listen, *base)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(fmt.Errorf("serve: %w", err))
	}
}

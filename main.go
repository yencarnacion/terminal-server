package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"
)

//go:embed quotes.txt
var bundledQuotes string

func main() {
	cfg, err := readConfiguration(os.Args[1:], os.Stderr)
	if err == flag.ErrHelp {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		log.Fatal(err)
	}
	content := []byte(bundledQuotes)
	if cfg.QuotesFile != "" {
		content, err = os.ReadFile(cfg.QuotesFile)
		if err != nil {
			log.Fatal(err)
		}
	}
	fortunes, err := parseQuotes(string(content))
	if err != nil {
		log.Fatal(err)
	}
	key, err := loadKey(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	app, err := newApp(strings.TrimRight(cfg.BaseURL, "/"), cfg.SlideSeconds, cfg.Screen, fortunes, key)
	if err != nil {
		log.Fatal(err)
	}
	app.news = newNews(cfg.RSSURL)
	app.location = location
	app.slideOrder = append([]string(nil), cfg.SlideOrder...)
	app.polymarket, err = newPolymarket(cfg.PolymarketPages)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.WeatherEnabled {
		app.weather = newWeather(cfg)
	}
	if cfg.FrontpagesURL != "" {
		app.frontpages, err = newFrontpages(cfg.FrontpagesURL)
		if err != nil {
			log.Fatal(err)
		}
	}
	server := &http.Server{Addr: cfg.Listen, Handler: app.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if app.news != nil {
		go app.news.run(ctx)
	}
	if app.frontpages != nil {
		go app.frontpages.run(ctx)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("TRMNL X: %d quotes, screen=%s, slide_seconds=%d, timezone=%s, listen=%s, preview=%s/preview", len(fortunes), cfg.Screen, cfg.SlideSeconds, cfg.Timezone, cfg.Listen, cfg.BaseURL)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(fmt.Errorf("serve: %w", err))
	}
}

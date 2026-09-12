package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type configuration struct {
	Listen        string `yaml:"listen"`
	BaseURL       string `yaml:"base_url"`
	SlideSeconds  int    `yaml:"slide_seconds"`
	Screen        string `yaml:"screen"`
	Timezone      string `yaml:"timezone"`
	QuotesFile    string `yaml:"quotes_file"`
	DataDir       string `yaml:"data_dir"`
	FrontpagesURL string `yaml:"frontpages_url"`
}

func defaultConfiguration() configuration {
	return configuration{Listen: ":8177", BaseURL: "http://10.17.17.90:8177", SlideSeconds: 120, Screen: "slideshow", Timezone: "America/New_York", DataDir: "./data", FrontpagesURL: "http://10.17.17.90:8100"}
}

// Precedence: built-in defaults < YAML values < explicitly supplied CLI flags.
// Paths are relative to the working directory, preserving existing installations.
func readConfiguration(args []string, output io.Writer) (configuration, error) {
	cfg := defaultConfiguration()
	cli := cfg
	fs := flag.NewFlagSet("terminal-server", flag.ContinueOnError)
	fs.SetOutput(output)
	path := fs.String("config", "config.yaml", "YAML settings file; empty disables file loading")
	fs.StringVar(&cli.Listen, "listen", cli.Listen, "HTTP bind address")
	fs.StringVar(&cli.BaseURL, "base-url", cli.BaseURL, "URL reachable by the device")
	fs.IntVar(&cli.SlideSeconds, "refresh", cli.SlideSeconds, "seconds each slideshow screen is displayed")
	fs.StringVar(&cli.Screen, "screen", cli.Screen, "slideshow, cowsay, calendar, or quarter")
	fs.StringVar(&cli.Timezone, "timezone", cli.Timezone, "IANA timezone")
	fs.StringVar(&cli.QuotesFile, "quotes", cli.QuotesFile, "fortune-format file; empty uses bundled quotes")
	fs.StringVar(&cli.DataDir, "data-dir", cli.DataDir, "persistent device key directory")
	fs.StringVar(&cli.FrontpagesURL, "frontpages-url", cli.FrontpagesURL, "frontpages origin; empty disables newspaper slides")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if *path != "" {
		data, err := os.ReadFile(*path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) || explicit["config"] {
				return cfg, fmt.Errorf("read config %s: %w", *path, err)
			}
		} else {
			dec := yaml.NewDecoder(strings.NewReader(string(data)))
			dec.KnownFields(true)
			if err := dec.Decode(&cfg); err != nil && err != io.EOF {
				return cfg, fmt.Errorf("decode config %s: %w", *path, err)
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				return cfg, fmt.Errorf("config %s must contain a single YAML document", *path)
			}
		}
	}
	for name := range explicit {
		switch name {
		case "listen":
			cfg.Listen = cli.Listen
		case "base-url":
			cfg.BaseURL = cli.BaseURL
		case "refresh":
			cfg.SlideSeconds = cli.SlideSeconds
		case "screen":
			cfg.Screen = cli.Screen
		case "timezone":
			cfg.Timezone = cli.Timezone
		case "quotes":
			cfg.QuotesFile = cli.QuotesFile
		case "data-dir":
			cfg.DataDir = cli.DataDir
		case "frontpages-url":
			cfg.FrontpagesURL = cli.FrontpagesURL
		}
	}
	return cfg, cfg.validate()
}

func validOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Host != "" && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

func (c configuration) validate() error {
	if c.SlideSeconds < 60 {
		return fmt.Errorf("slide_seconds / --refresh must be at least 60 seconds")
	}
	if !validOrigin(c.BaseURL) {
		return fmt.Errorf("base_url / --base-url must be an absolute HTTP(S) origin")
	}
	if c.FrontpagesURL != "" && !validOrigin(c.FrontpagesURL) {
		return fmt.Errorf("frontpages_url / --frontpages-url must be an absolute HTTP(S) origin or empty")
	}
	if c.Screen != "slideshow" && c.Screen != "cowsay" && c.Screen != "calendar" && c.Screen != "quarter" {
		return fmt.Errorf("unknown screen %q", c.Screen)
	}
	if c.Timezone == "" {
		return fmt.Errorf("timezone must not be empty")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be host:port or :port: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("listen port must be between 1 and 65535")
	}
	return nil
}

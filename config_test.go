package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func configFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigurationDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := readConfiguration(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, defaultConfiguration()) {
		t.Fatalf("%+v", cfg)
	}
	if cfg.SlideSeconds != 60 {
		t.Fatalf("default slide duration = %d, want 60", cfg.SlideSeconds)
	}
	if _, err = readConfiguration([]string{"--config", "missing.yaml"}, io.Discard); err == nil {
		t.Fatal("explicit missing config ignored")
	}
	if _, err = readConfiguration([]string{"--help"}, io.Discard); err != flag.ErrHelp {
		t.Fatal(err)
	}
}

func TestShippedConfiguration(t *testing.T) {
	cfg, err := readConfiguration([]string{"--config", "config.yaml"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, defaultConfiguration()) {
		t.Fatalf("shipped config diverges from defaults: %+v", cfg)
	}
}

func TestConfigurationFileAndOverrides(t *testing.T) {
	path := configFile(t, `slide_seconds: 300
listen: ":8188"
base_url: "http://example.local:8188"
screen: quarter
timezone: Europe/London
quotes_file: ./custom.txt
data_dir: ./test-data
frontpages_url: ""
`)
	cfg, err := readConfiguration([]string{"--config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SlideSeconds != 300 || cfg.Listen != ":8188" || cfg.BaseURL != "http://example.local:8188" || cfg.Screen != "quarter" || cfg.Timezone != "Europe/London" || cfg.QuotesFile != "./custom.txt" || cfg.DataDir != "./test-data" || cfg.FrontpagesURL != "" {
		t.Fatalf("%+v", cfg)
	}
	cfg, err = readConfiguration([]string{"--refresh", "420", "--config", path, "--screen", "slideshow", "--quotes", "", "--frontpages-url", "http://news.local:8100"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SlideSeconds != 420 || cfg.Screen != "slideshow" || cfg.QuotesFile != "" || cfg.FrontpagesURL != "http://news.local:8100" || cfg.Listen != ":8188" {
		t.Fatalf("%+v", cfg)
	}
	cfg, err = readConfiguration([]string{"--config", "", "--refresh", "240"}, io.Discard)
	if err != nil || cfg.SlideSeconds != 240 || cfg.Listen != ":8177" {
		t.Fatal(cfg, err)
	}
}

func TestConfigurationRejectsMistakes(t *testing.T) {
	for _, content := range []string{
		"slide_second: 300\n", "slide_seconds: nope\n", "slide_seconds: 5\n", "slide_seconds: 0\n",
		"slide_seconds: 180\nslide_seconds: 300\n", "slide_seconds: 180\n---\nslide_seconds: 300\n",
		"screen: missing\n", "timezone: Invalid/Zone\n", "timezone: ''\n",
		"listen: ':99999'\n", "base_url: /relative\n", "frontpages_url: ftp://example.com\n", "data_dir: ''\n",
	} {
		t.Run(strings.TrimSpace(content), func(t *testing.T) {
			if _, err := readConfiguration([]string{"--config", configFile(t, content)}, io.Discard); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	// CLI flags may override invalid file values before final validation.
	cfg, err := readConfiguration([]string{"--config", configFile(t, "slide_seconds: 1\n"), "--refresh", "180"}, io.Discard)
	if err != nil || cfg.SlideSeconds != 180 {
		t.Fatal(cfg, err)
	}
}

package main

import (
	"os"
	"testing"
)

func TestLoadConfig_Defaults(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cfg*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("admin_username: admin\nadmin_password_hash: $2a$10$xxx\nencryption_key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
	f.Close()

	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.S3Listen != ":9000" {
		t.Errorf("S3Listen default: got %q want %q", cfg.S3Listen, ":9000")
	}
	if cfg.AdminListen != ":9001" {
		t.Errorf("AdminListen default: got %q want %q", cfg.AdminListen, ":9001")
	}
	if cfg.BucketCacheSize != 50 {
		t.Errorf("BucketCacheSize default: got %d want 50", cfg.BucketCacheSize)
	}
	if cfg.SyncInterval != "5m" {
		t.Errorf("SyncInterval default: got %q want %q", cfg.SyncInterval, "5m")
	}
	if cfg.StatsFlusInterval != "1m" {
		t.Errorf("StatsFlusInterval default: got %q want %q", cfg.StatsFlusInterval, "1m")
	}
	if cfg.Region != "us-east-1" {
		t.Errorf("Region default: got %q want %q", cfg.Region, "us-east-1")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat default: got %q want %q", cfg.LogFormat, "json")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default: got %q want %q", cfg.LogLevel, "info")
	}
}

func TestLoadConfig_Override(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cfg*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`
s3_listen: ":8080"
admin_listen: ":8081"
admin_username: root
admin_password_hash: hash
encryption_key: key
bucket_cache_size: 10
sync_interval: "2m"
stats_flush_interval: "30s"
region: eu-west-1
log_format: json
log_level: debug
`)
	f.Close()

	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.S3Listen != ":8080" {
		t.Errorf("S3Listen: got %q", cfg.S3Listen)
	}
	if cfg.Region != "eu-west-1" {
		t.Errorf("Region: got %q", cfg.Region)
	}
	if cfg.BucketCacheSize != 10 {
		t.Errorf("BucketCacheSize: got %d", cfg.BucketCacheSize)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	// Missing config file is not an error — daemon can run on ENV vars alone.
	cfg, err := LoadConfig("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error for missing file: %v", err)
	}
	if cfg.S3Listen != ":9000" {
		t.Errorf("expected default S3Listen :9000, got %s", cfg.S3Listen)
	}
}

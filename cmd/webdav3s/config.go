package main

import (
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	S3Listen          string `yaml:"s3_listen"`
	AdminListen       string `yaml:"admin_listen"`
	AdminUsername     string `yaml:"admin_username"`
	AdminPasswordHash string `yaml:"admin_password_hash"`
	EncryptionKey     string `yaml:"encryption_key"`
	LocalCacheDir     string `yaml:"local_cache_dir"`
	ProvisionFile     string `yaml:"-"`
	BucketCacheSize   int    `yaml:"bucket_cache_size"`
	SyncInterval      string `yaml:"sync_interval"`
	StatsFlusInterval string `yaml:"stats_flush_interval"`
	DaemonID          string `yaml:"daemon_id"`
	LogFormat         string `yaml:"log_format"`
	LogLevel          string `yaml:"log_level"`
	Region            string `yaml:"region"`
	ChunkThresholdMB  int    `yaml:"chunk_threshold_mb"`
	ChunkSizeMB       int    `yaml:"chunk_size_mb"`
}

func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		S3Listen:          ":9000",
		AdminListen:       ":9001",
		BucketCacheSize:   50,
		SyncInterval:      "5m",
		StatsFlusInterval: "1m",
		Region:            "us-east-1",
		LogFormat:         "json",
		LogLevel:          "info",
		ChunkSizeMB:       100,
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, err
			}
		}
	}

	// Environment variables override YAML (useful for Docker / container deployments).
	// Prefix: WEBDAV3S_
	overrideStr := func(field *string, env string) {
		if v := os.Getenv(env); v != "" {
			*field = v
		}
	}
	overrideInt := func(field *int, env string) {
		if v := os.Getenv(env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*field = n
			}
		}
	}

	overrideStr(&cfg.S3Listen, "WEBDAV3S_S3_LISTEN")
	overrideStr(&cfg.AdminListen, "WEBDAV3S_ADMIN_LISTEN")
	overrideStr(&cfg.AdminUsername, "WEBDAV3S_ADMIN_USERNAME")
	overrideStr(&cfg.AdminPasswordHash, "WEBDAV3S_ADMIN_PASSWORD_HASH")
	overrideStr(&cfg.EncryptionKey, "WEBDAV3S_ENCRYPTION_KEY")
	overrideStr(&cfg.LocalCacheDir, "WEBDAV3S_LOCAL_CACHE_DIR")
	overrideStr(&cfg.ProvisionFile, "WEBDAV3S_PROVISION_FILE")
	overrideInt(&cfg.BucketCacheSize, "WEBDAV3S_BUCKET_CACHE_SIZE")
	overrideStr(&cfg.SyncInterval, "WEBDAV3S_SYNC_INTERVAL")
	overrideStr(&cfg.StatsFlusInterval, "WEBDAV3S_STATS_FLUSH_INTERVAL")
	overrideStr(&cfg.DaemonID, "WEBDAV3S_DAEMON_ID")
	overrideStr(&cfg.LogFormat, "WEBDAV3S_LOG_FORMAT")
	overrideStr(&cfg.LogLevel, "WEBDAV3S_LOG_LEVEL")
	overrideStr(&cfg.Region, "WEBDAV3S_REGION")
	overrideInt(&cfg.ChunkThresholdMB, "WEBDAV3S_CHUNK_THRESHOLD_MB")
	overrideInt(&cfg.ChunkSizeMB, "WEBDAV3S_CHUNK_SIZE_MB")

	return cfg, nil
}

package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

// runSetup runs the interactive first-run setup wizard and writes config.yaml.
func runSetup(cfgPath string) error {
	if _, err := os.Stat(cfgPath); err == nil {
		fmt.Fprintf(os.Stderr, "config file %s already exists — delete it first to re-run setup\n", cfgPath)
		os.Exit(1)
	}

	fmt.Println("webdavs3 setup")
	fmt.Println("==============")
	fmt.Println()

	// Admin username
	username := prompt("Admin username [admin]: ")
	if username == "" {
		username = "admin"
	}

	// Admin password (hidden input, confirmed)
	password, err := promptPassword("Admin password: ")
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	confirm, err := promptPassword("Confirm password: ")
	if err != nil {
		return fmt.Errorf("read password confirmation: %w", err)
	}
	if password != confirm {
		fmt.Fprintln(os.Stderr, "passwords do not match")
		os.Exit(1)
	}
	if len(password) < 8 {
		fmt.Fprintln(os.Stderr, "password must be at least 8 characters")
		os.Exit(1)
	}

	fmt.Print("Hashing password...")
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}
	fmt.Println(" done")

	// Generate AES-256 encryption key
	var keyBytes [32]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		return fmt.Errorf("generate encryption key: %w", err)
	}
	encKey := base64.StdEncoding.EncodeToString(keyBytes[:])

	// Write config.yaml
	cfg := fmt.Sprintf(`s3_listen: ":9000"
admin_listen: ":9001"

admin_username: %q
admin_password_hash: %q

# AES-256-GCM key — encrypts WebDAV passwords and S3 user secrets at rest.
# Keep this secret. Loss means all stored credentials must be re-entered.
encryption_key: %q

# Local directory for SQLite cache files and daemon.id.
local_cache_dir: ""

# S3 region returned to clients.
region: "us-east-1"

# Maximum number of bucket databases to keep open simultaneously (LRU).
bucket_cache_size: 50

# How often to pull metadata from WebDAV.
sync_interval: "5m"

# How often to flush local usage stats to WebDAV.
stats_flush_interval: "1m"

# log_format: json | text
# log_level:  debug | info | warn | error
log_format: "json"
log_level: "info"
`, username, string(hash), encKey)

	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}

	fmt.Printf("\nConfig written to %s\n", cfgPath)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  1. Start the daemon:  webdavs3 %s\n", cfgPath)
	fmt.Printf("  2. Open the admin UI: http://localhost:9001/admin/\n")
	fmt.Println("  3. Add a WebDAV location, then create S3 users.")
	return nil
}

func prompt(label string) string {
	fmt.Print(label)
	var line string
	fmt.Scanln(&line)
	return line
}

func promptPassword(label string) (string, error) {
	fmt.Print(label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

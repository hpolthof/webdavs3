package provisioning

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/hpolthof/webdavs3/internal/meta"
	"gopkg.in/yaml.v3"
)

type Document struct {
	Users     []UserSpec     `yaml:"users"`
	Locations []LocationSpec `yaml:"locations"`
}

type UserSpec struct {
	Slug        string `yaml:"slug"`
	DisplayName string `yaml:"display_name"`
	AccessKey   string `yaml:"access_key"`
	SecretKey   string `yaml:"secret_key"`
	WebPassword string `yaml:"web_password"`
	Enabled     *bool  `yaml:"enabled,omitempty"`
}

type LocationSpec struct {
	Slug        string       `yaml:"slug"`
	DisplayName string       `yaml:"display_name"`
	URL         string       `yaml:"url"`
	Username    string       `yaml:"username"`
	Password    string       `yaml:"password"`
	QuotaGB     int64        `yaml:"quota_gb,omitempty"`
	BaseDir     string       `yaml:"base_dir,omitempty"`
	Buckets     []BucketSpec `yaml:"buckets"`
}

type BucketSpec struct {
	Name  string `yaml:"name"`
	Owner string `yaml:"owner"`
}

func ParseFile(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}

	var doc Document
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Document{}, err
	}

	return doc, nil
}

func (d *Document) Normalize() {
	for i := range d.Users {
		if d.Users[i].Enabled == nil {
			enabled := true
			d.Users[i].Enabled = &enabled
		}
	}

	for i := range d.Locations {
		d.Locations[i].BaseDir = meta.NormalizeBaseDir(d.Locations[i].BaseDir)
	}
}

var slugPattern = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugPattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "item"
	}
	return s
}

func uniqueSlug(base string, seen map[string]int) string {
	if _, ok := seen[base]; !ok {
		seen[base] = 1
		return base
	}
	seen[base]++
	return fmt.Sprintf("%s-%d", base, seen[base])
}

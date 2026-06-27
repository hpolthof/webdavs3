package provisioning

import (
	"fmt"
	"strings"
)

func Validate(doc Document) error {
	userSlugs := map[string]struct{}{}
	accessKeys := map[string]struct{}{}
	bucketNames := map[string]struct{}{}

	for _, user := range doc.Users {
		userSlug := strings.TrimSpace(user.Slug)
		if userSlug == "" {
			return fmt.Errorf("users.slug is required")
		}
		if strings.TrimSpace(user.DisplayName) == "" {
			return fmt.Errorf("users.display_name is required")
		}
		accessKey := strings.TrimSpace(user.AccessKey)
		if accessKey == "" {
			return fmt.Errorf("users.access_key is required")
		}
		if strings.TrimSpace(user.SecretKey) == "" {
			return fmt.Errorf("users.secret_key is required")
		}
		if strings.TrimSpace(user.WebPassword) == "" {
			return fmt.Errorf("users.web_password is required")
		}
		if _, ok := userSlugs[userSlug]; ok {
			return fmt.Errorf("duplicate user slug %q", userSlug)
		}
		userSlugs[userSlug] = struct{}{}

		if _, ok := accessKeys[accessKey]; ok {
			return fmt.Errorf("duplicate access key %q", accessKey)
		}
		accessKeys[accessKey] = struct{}{}
	}

	locationSlugs := map[string]struct{}{}
	for _, loc := range doc.Locations {
		locationSlug := strings.TrimSpace(loc.Slug)
		if locationSlug == "" {
			return fmt.Errorf("locations.slug is required")
		}
		if strings.TrimSpace(loc.DisplayName) == "" {
			return fmt.Errorf("locations.display_name is required")
		}
		if strings.TrimSpace(loc.URL) == "" {
			return fmt.Errorf("locations.url is required")
		}
		if strings.TrimSpace(loc.Username) == "" {
			return fmt.Errorf("locations.username is required")
		}
		if strings.TrimSpace(loc.Password) == "" {
			return fmt.Errorf("locations.password is required")
		}
		if _, ok := locationSlugs[locationSlug]; ok {
			return fmt.Errorf("duplicate location slug %q", locationSlug)
		}
		locationSlugs[locationSlug] = struct{}{}

		for _, bucket := range loc.Buckets {
			bucketName := strings.TrimSpace(bucket.Name)
			if bucketName == "" {
				return fmt.Errorf("buckets.name is required")
			}
			bucketOwner := strings.TrimSpace(bucket.Owner)
			if bucketOwner == "" {
				return fmt.Errorf("buckets.owner is required")
			}
			if _, ok := userSlugs[bucketOwner]; !ok {
				return fmt.Errorf("bucket %q references unknown owner %q", bucketName, bucketOwner)
			}
			if _, ok := bucketNames[bucketName]; ok {
				return fmt.Errorf("duplicate bucket name %q", bucketName)
			}
			bucketNames[bucketName] = struct{}{}
		}
	}

	return nil
}

package domains

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type domainList struct {
	CacheDomains []struct {
		Name        string   `json:"name"`
		DomainFiles []string `json:"domain_files"`
	} `json:"cache_domains"`
}

// LoadDir builds a Matcher from a cache-domains checkout in dir. Malformed
// individual lines are skipped and reported in skipped; a missing or invalid
// cache_domains.json is an error.
func LoadDir(dir string) (m *Matcher, skipped []string, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, "cache_domains.json"))
	if err != nil {
		return nil, nil, err
	}
	var list domainList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil, fmt.Errorf("cache_domains.json: %w", err)
	}
	m = NewMatcher()
	for _, cd := range list.CacheDomains {
		for _, file := range cd.DomainFiles {
			if !filepath.IsLocal(file) {
				skipped = append(skipped, fmt.Sprintf("%s: unsafe path %q", cd.Name, file))
				continue
			}
			s, err := addFile(m, cd.Name, filepath.Join(dir, file))
			if err != nil {
				return nil, nil, err
			}
			skipped = append(skipped, s...)
		}
	}
	return m, skipped, nil
}

func addFile(m *Matcher, name, path string) (skipped []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := m.Add(name, line); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", filepath.Base(path), err))
		}
	}
	return skipped, sc.Err()
}

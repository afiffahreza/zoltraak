package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
)

// pkg is one downloadable .deb, as listed in a Packages index.
type pkg struct {
	path string // Filename field, relative to the archive root
	size int64  // Size field, bytes
}

// loadPool downloads the Packages.gz index for every suite/component and
// returns the de-duplicated set of packages with size <= maxSize (0 = no limit).
func loadPool(ctx context.Context, c *http.Client, cfg config) ([]pkg, error) {
	seen := make(map[string]struct{})
	var pool []pkg
	for _, suite := range cfg.suites {
		for _, comp := range cfg.components {
			url := fmt.Sprintf("%s/dists/%s/%s/binary-%s/Packages.gz", cfg.indexBase, suite, comp, cfg.arch)
			pkgs, err := fetchIndex(ctx, c, url)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", url, err)
			}
			added := 0
			for _, p := range pkgs {
				if cfg.maxSize > 0 && p.size > cfg.maxSize {
					continue
				}
				if _, dup := seen[p.path]; dup {
					continue
				}
				seen[p.path] = struct{}{}
				pool = append(pool, p)
				added++
			}
			fmt.Printf("index %-18s %-11s %6d packages (%d new)\n", suite, comp, len(pkgs), added)
		}
	}
	return pool, nil
}

func shuffle(pool []pkg, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0))
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
}

func fetchIndex(ctx context.Context, c *http.Client, url string) ([]pkg, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return parsePackages(zr)
}

// parsePackages extracts Filename/Size from a Debian control-format stream.
func parsePackages(r io.Reader) ([]pkg, error) {
	var out []pkg
	var cur pkg
	flush := func() {
		if cur.path != "" {
			out = append(out, cur)
		}
		cur = pkg{}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // Description fields can be long
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "Filename: "):
			cur.path = strings.TrimPrefix(line, "Filename: ")
		case strings.HasPrefix(line, "Size: "):
			n, err := strconv.ParseInt(strings.TrimPrefix(line, "Size: "), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad Size line %q: %w", line, err)
			}
			cur.size = n
		}
	}
	flush()
	return out, sc.Err()
}

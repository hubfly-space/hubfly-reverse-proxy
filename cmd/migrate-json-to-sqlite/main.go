package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
	"github.com/hubfly/hubfly-reverse-proxy/internal/store"
)

func main() {
	inputDir := flag.String("input-dir", ".", "Directory containing legacy sites.json/streams.json/metadata.json")
	outputDir := flag.String("output-dir", ".", "Hubfly runtime directory where data/http and data/tcp will be created")
	flag.Parse()

	in, err := filepath.Abs(*inputDir)
	if err != nil {
		fail("resolve input-dir", err)
	}
	out, err := filepath.Abs(*outputDir)
	if err != nil {
		fail("resolve output-dir", err)
	}

	sites, err := loadSites(in)
	if err != nil {
		fail("load legacy sites", err)
	}
	streams, err := loadStreams(in)
	if err != nil {
		fail("load legacy streams", err)
	}

	st, err := store.NewSQLiteStore(out)
	if err != nil {
		fail("init sqlite store", err)
	}

	for _, site := range sites {
		s := site
		if err := st.SaveSite(&s); err != nil {
			fail("save site", err)
		}
	}
	for _, stream := range streams {
		s := stream
		if err := st.SaveStream(&s); err != nil {
			fail("save stream", err)
		}
	}

	fmt.Printf("migration complete: sites=%d streams=%d output=%s\n", len(sites), len(streams), out)
}

func loadSites(dir string) ([]models.Site, error) {
	paths := []string{
		filepath.Join(dir, "sites.json"),
		filepath.Join(dir, "metadata.json"),
	}
	for _, p := range paths {
		out, found, err := loadJSONMap[models.Site](p)
		if err != nil {
			return nil, err
		}
		if found {
			return out, nil
		}
	}
	return []models.Site{}, nil
}

func loadStreams(dir string) ([]models.Stream, error) {
	out, _, err := loadJSONMap[models.Stream](filepath.Join(dir, "streams.json"))
	return out, err
}

func loadJSONMap[T any](path string) ([]T, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(b) == 0 {
		return []T{}, true, nil
	}
	m := make(map[string]T)
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, true, fmt.Errorf("decode %s: %w", path, err)
	}
	out := make([]T, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out, true, nil
}

func fail(action string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
	os.Exit(1)
}

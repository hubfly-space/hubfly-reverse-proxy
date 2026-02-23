package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

type Store interface {
	ListSites() ([]models.Site, error)
	GetSite(id string) (*models.Site, error)
	SaveSite(site *models.Site) error
	DeleteSite(id string) error

	ListStreams() ([]models.Stream, error)
	GetStream(id string) (*models.Stream, error)
	SaveStream(stream *models.Stream) error
	DeleteStream(id string) error
}

type JSONStore struct {
	sitesFilePath       string
	legacySitesFilePath string
	streamsFilePath     string
	mu                  sync.RWMutex
	sites               map[string]models.Site
	streams             map[string]models.Stream
}

func NewJSONStore(dir string) (*JSONStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	s := &JSONStore{
		sitesFilePath:       filepath.Join(dir, "sites.json"),
		legacySitesFilePath: filepath.Join(dir, "metadata.json"),
		streamsFilePath:     filepath.Join(dir, "streams.json"),
		sites:               make(map[string]models.Site),
		streams:             make(map[string]models.Stream),
	}

	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *JSONStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	siteFileToLoad := s.sitesFilePath
	if _, err := os.Stat(siteFileToLoad); os.IsNotExist(err) {
		if _, legacyErr := os.Stat(s.legacySitesFilePath); legacyErr == nil {
			siteFileToLoad = s.legacySitesFilePath
		}
	}

	// Load Sites
	if data, err := os.ReadFile(siteFileToLoad); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &s.sites); err != nil {
			return fmt.Errorf("failed to load sites: %w", err)
		}
	}

	// Load Streams
	if data, err := os.ReadFile(s.streamsFilePath); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &s.streams); err != nil {
			return fmt.Errorf("failed to load streams: %w", err)
		}
	}

	return nil
}

func (s *JSONStore) saveSites() error {
	data, err := json.MarshalIndent(s.sites, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.sitesFilePath, data, 0644)
}

func (s *JSONStore) saveStreams() error {
	data, err := json.MarshalIndent(s.streams, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.streamsFilePath, data, 0644)
}

func (s *JSONStore) ListSites() ([]models.Site, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]models.Site, 0, len(s.sites))
	for _, site := range s.sites {
		list = append(list, site)
	}
	return list, nil
}

func (s *JSONStore) GetSite(id string) (*models.Site, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	site, ok := s.sites[id]
	if !ok {
		return nil, fmt.Errorf("site not found: %s", id)
	}
	return &site, nil
}

func (s *JSONStore) SaveSite(site *models.Site) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sites[site.ID] = *site
	return s.saveSites()
}

func (s *JSONStore) DeleteSite(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.sites, id)
	return s.saveSites()
}

// Stream Methods

func (s *JSONStore) ListStreams() ([]models.Stream, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]models.Stream, 0, len(s.streams))
	for _, stream := range s.streams {
		list = append(list, stream)
	}
	return list, nil
}

func (s *JSONStore) GetStream(id string) (*models.Stream, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stream, ok := s.streams[id]
	if !ok {
		return nil, fmt.Errorf("stream not found: %s", id)
	}
	return &stream, nil
}

func (s *JSONStore) SaveStream(stream *models.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.streams[stream.ID] = *stream
	return s.saveStreams()
}

func (s *JSONStore) DeleteStream(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.streams, id)
	return s.saveStreams()
}

// saveAtomic is removed as it is no longer needed.

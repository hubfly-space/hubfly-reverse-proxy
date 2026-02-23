package containeripmap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store persists docker container name -> IP mappings to disk.
type Store struct {
	path string
	mu   sync.RWMutex
	data map[string]string
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, data: make(map[string]string)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}

	bytes, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(bytes) == 0 {
		return nil
	}

	return json.Unmarshal(bytes, &s.data)
}

func (s *Store) Save() error {
	s.mu.RLock()
	copyMap := make(map[string]string, len(s.data))
	for k, v := range s.data {
		copyMap[k] = v
	}
	s.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}

	ordered := make(map[string]string, len(copyMap))
	keys := make([]string, 0, len(copyMap))
	for k := range copyMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ordered[k] = copyMap[k]
	}

	data, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *Store) Set(name, ip string) (bool, error) {
	s.mu.Lock()
	current, ok := s.data[name]
	if ok && current == ip {
		s.mu.Unlock()
		return false, nil
	}
	s.data[name] = ip
	s.mu.Unlock()

	if err := s.Save(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) GetAll() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	copyMap := make(map[string]string, len(s.data))
	for k, v := range s.data {
		copyMap[k] = v
	}
	return copyMap
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

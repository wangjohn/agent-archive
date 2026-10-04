package state

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
)

const listingRepairDir = "listing-repairs"

// ListingRepair is durable auxiliary work, bound to its admitted destination.
// It holds no source content and cannot keep a transcript alive.
type ListingRepair struct {
	MetadataKey   string `json:"metadata_key"`
	DestinationID string `json:"destination_id"`
}

// SaveListingRepair journals work before canonical publication starts.
func (s *Store) SaveListingRepair(id string, repair ListingRepair) error {
	if !safeFileComponent(id) {
		return errors.New("invalid repair session ID")
	}
	return local.Write(filepath.Join(s.home, listingRepairDir, id+".json"), repair)
}

// RemoveListingRepair acknowledges successful or obsolete auxiliary work.
func (s *Store) RemoveListingRepair(id string) error {
	if !safeFileComponent(id) {
		return errors.New("invalid repair session ID")
	}
	err := os.Remove(filepath.Join(s.home, listingRepairDir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ListingRepairs returns at most limit intents for one collector slice.
func (s *Store) ListingRepairs(limit int) (map[string]ListingRepair, error) {
	entries, err := os.ReadDir(filepath.Join(s.home, listingRepairDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cursor string
	_ = local.Read(filepath.Join(s.home, listingRepairDir, "cursor"), &cursor)
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].Name() > cursor, entries[j].Name() > cursor
		if a != b {
			return a
		}
		return entries[i].Name() < entries[j].Name()
	})
	repairs := make(map[string]ListingRepair)
	last := ""
	for _, entry := range entries {
		if len(repairs) >= limit {
			break
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5]
		if !safeFileComponent(id) {
			continue
		}
		var repair ListingRepair
		if _, err := s.readOwned(filepath.Join(s.home, listingRepairDir, entry.Name()), &repair); err != nil {
			return nil, err
		}
		repairs[id] = repair
		last = entry.Name()
	}
	if strings.TrimSpace(last) != "" {
		if err := local.Write(filepath.Join(s.home, listingRepairDir, "cursor"), last); err != nil {
			return nil, err
		}
	}
	return repairs, nil
}

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The four sidecar files the desktop used before Plan 11. Each was a flat map
// keyed by transcript base name, written and read only by the desktop.
const (
	legacyTitles   = ".titles.json"
	legacyDisplays = ".display.json"
	legacyCwds     = ".cwds.json"
	legacyKinds    = ".kinds.json"
)

// importLegacy rebuilds the index from the old sidecars on the first run after
// the upgrade. Caller holds mu.
//
// The sidecars are deliberately **left in place**. They are small, and leaving
// them means a user who downgrades still has their session titles — losing
// someone's named conversations to a refactor would be a poor trade for tidiness.
// The index wins from here on: nothing reads the sidecars again once it exists.
func (r *Registry) importLegacy() {
	titles := readLegacyMap(filepath.Join(r.dir, legacyTitles))
	cwds := readLegacyMap(filepath.Join(r.dir, legacyCwds))
	kinds := readLegacyMap(filepath.Join(r.dir, legacyKinds))
	displays := readLegacyDisplays(filepath.Join(r.dir, legacyDisplays))
	if len(titles)+len(cwds)+len(kinds)+len(displays) == 0 {
		return
	}

	// A record's timestamps come from its transcript when one is still there —
	// the sidecars never carried any.
	stat := func(base string) (created, updated time.Time) {
		info, err := os.Stat(filepath.Join(r.dir, base))
		if err != nil {
			now := time.Now().UTC()
			return now, now
		}
		return info.ModTime().UTC(), info.ModTime().UTC()
	}

	seen := map[string]bool{}
	for _, m := range []map[string]string{titles, cwds, kinds} {
		for base := range m {
			seen[base] = true
		}
	}
	for base := range displays {
		seen[base] = true
	}
	for base := range seen {
		created, updated := stat(base)
		store := filepath.Join(r.dir, base)
		r.put(Record{
			ID: newID(base), Engine: EngineNative, Store: store,
			Workspace: cwds[base], Title: titles[base], Kind: kinds[base],
			CreatedAt: created, UpdatedAt: updated, Display: displays[base],
		})
	}
	// Best effort: a failed write just means the import runs again next time.
	_ = r.save()
}

func readLegacyMap(path string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func readLegacyDisplays(path string) map[string]map[string]string {
	m := map[string]map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

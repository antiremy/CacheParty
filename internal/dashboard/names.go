package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// gameNames affects presentation only. Traffic and purge identities stay depot IDs.
type gameNames struct {
	Names       map[string]map[string]map[string]string `json:"names"`
	SteamAppIDs map[string]string                       `json:"steamAppIds"`
}

var numericGameID = regexp.MustCompile(`^[0-9]{1,20}$`)

func emptyGameNames() gameNames {
	return gameNames{Names: map[string]map[string]map[string]string{}, SteamAppIDs: map[string]string{}}
}

func readGameNames(path string) (gameNames, error) {
	f, err := os.Open(path)
	if err != nil {
		return gameNames{}, err
	}
	defer f.Close()
	// Read one extra byte so a valid JSON prefix cannot hide an oversized file.
	const maxSize = 4 << 20
	body, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return gameNames{}, err
	}
	if len(body) > maxSize {
		return gameNames{}, os.ErrInvalid
	}
	var names gameNames
	if err := json.Unmarshal(body, &names); err != nil {
		return gameNames{}, err
	}
	if names.Names == nil {
		return gameNames{}, os.ErrInvalid
	}
	for _, kinds := range names.Names {
		if kinds == nil {
			return gameNames{}, os.ErrInvalid
		}
		for _, entries := range kinds {
			if entries == nil {
				return gameNames{}, os.ErrInvalid
			}
			for id, title := range entries {
				if len(id) == 0 || len(id) > 200 || strings.TrimSpace(title) == "" || len(title) > 2000 {
					return gameNames{}, os.ErrInvalid
				}
			}
		}
	}
	for depot, owner := range names.SteamAppIDs {
		if !numericGameID.MatchString(depot) || !numericGameID.MatchString(owner) {
			return gameNames{}, os.ErrInvalid
		}
		// Group only when an explicit title and a verified owner are both supplied.
		if strings.TrimSpace(names.Names["steam"]["depot"][depot]) == "" {
			return gameNames{}, os.ErrInvalid
		}
	}
	if names.SteamAppIDs == nil {
		names.SteamAppIDs = map[string]string{}
	}
	return names, nil
}

func (d *Dashboard) serveGameNames(w http.ResponseWriter, r *http.Request) {
	d.namesMu.Lock()
	defer d.namesMu.Unlock()
	if d.names.Names == nil {
		d.names = emptyGameNames()
	}
	if d.cfg.GameNamesFile != "" {
		if names, err := readGameNames(d.cfg.GameNamesFile); err == nil {
			d.names = names
		}
		// A writer or source failure must not erase the last valid snapshot.
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(d.names)
}

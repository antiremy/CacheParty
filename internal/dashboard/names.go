package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Game names put a name next to the IDs in download URLs (GAME_NAMES).
// Blizzard product codes are listed below. Steam depots are looked up in
// lancache-pics, a depot-to-game map built from Steam's own metadata and
// published as a GitHub release. It is downloaded while GAME_NAMES is on,
// checked for a new release once a day and kept in GAME_NAMES_FILE, so a
// restart does not download it again. Names are for display only: traffic
// totals, cache tags and purges keep using the IDs.

// steamRelease is a variable so tests can point it at a local server.
var steamRelease = "https://api.github.com/repos/regix1/lancache-pics/releases/latest"

const (
	steamAsset      = "pics_depot_mappings.json"
	steamCheckEvery = 24 * time.Hour
	steamRetryAfter = time.Hour
	maxSteamMap     = 1 << 30 // bytes; the map was 62 MB in November 2026
	maxNameLen      = 200
)

// minSteamDepots rejects a map too small to be a whole one, such as an
// empty file from a broken release. Tests lower it.
var minSteamDepots = 10000

// blizzardNames maps the CDN path in Blizzard download URLs (/tpr/<path>/)
// to games, in lower case. The path is not always the product code
// (Overwatch is product "pro" but path "ovw"); each comes from the "Path"
// column of https://us.version.battle.net/<product>/cdns. Every World of
// Warcraft edition shares "wow".
var blizzardNames = map[string]string{
	"anbs":        "Diablo Immortal",
	"aqua":        "Avowed",
	"auks":        "Call of Duty",
	"b04":         "Call of Duty: Black Ops 4",
	"diablo3":     "Diablo III",
	"fenris":      "Diablo IV",
	"fore":        "Call of Duty: Vanguard",
	"hero-live-a": "Heroes of the Storm",
	"hs":          "Hearthstone",
	"lazr":        "Call of Duty: Modern Warfare 2 Campaign Remastered",
	"odin":        "Call of Duty: Modern Warfare",
	"osi":         "Diablo II: Resurrected",
	"ovw":         "Overwatch",
	"rtro":        "Blizzard Arcade Collection",
	"sc1live":     "StarCraft: Remastered",
	"sc2":         "StarCraft II",
	"scor":        "Sea of Thieves",
	"w1r-live":    "Warcraft: Remastered",
	"w2r-live":    "Warcraft II: Remastered",
	"wallaby":     "Crash Bandicoot 4: It's About Time",
	"war3":        "Warcraft III: Reforged",
	"wow":         "World of Warcraft",
	"zeus":        "Call of Duty: Black Ops Cold War",
}

// steamNames is the Steam map as kept in memory and in GAME_NAMES_FILE.
type steamNames struct {
	Release string            `json:"release"` // lancache-pics release tag
	Depots  map[uint32]uint32 `json:"depots"`  // depot to the app that owns it
	Apps    map[uint32]string `json:"apps"`    // app to its name
}

// names looks up game names and keeps the Steam map current.
type names struct {
	client *http.Client
	file   string
	log    *slog.Logger
	wake   chan struct{}

	mu    sync.RWMutex
	steam *steamNames // nil until loaded or downloaded
}

func newNames(client *http.Client, file string, log *slog.Logger) *names {
	return &names{client: client, file: file, log: log, wake: make(chan struct{}, 1)}
}

// lookup returns a's name, or "" if it is not known. For a Steam depot it
// also returns the app ID of the game that owns it.
func (n *names) lookup(service string, a app) (name, game string) {
	switch {
	case service == "blizzard" && a.Kind == "product":
		return blizzardNames[strings.ToLower(a.ID)], ""
	case service == "steam" && a.Kind == "depot":
		depot, err := strconv.ParseUint(a.ID, 10, 32)
		if err != nil {
			return "", ""
		}
		n.mu.RLock()
		defer n.mu.RUnlock()
		if n.steam == nil {
			return "", ""
		}
		owner, ok := n.steam.Depots[uint32(depot)]
		if !ok {
			return "", ""
		}
		return n.steam.Apps[owner], strconv.FormatUint(uint64(owner), 10)
	}
	return "", ""
}

// poke makes run look at the setting again now.
func (n *names) poke() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// run keeps the Steam map current while on reports true, and drops it from
// memory while it reports false. It returns when ctx ends.
func (n *names) run(ctx context.Context, on func() bool) {
	var next time.Time // when to check for a new release
	for {
		switch {
		case !on():
			n.set(nil)
			next = time.Time{}
		case next.IsZero():
			n.load()
			fallthrough
		case !time.Now().Before(next):
			if err := n.updateWhileOn(ctx, on); err != nil {
				if ctx.Err() != nil {
					return
				}
				if !on() {
					continue
				}
				n.log.Warn("game names: Steam names not updated", "err", err)
				next = time.Now().Add(steamRetryAfter)
			} else {
				next = time.Now().Add(steamCheckEvery)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
		case <-time.After(time.Hour):
		}
	}
}

func (n *names) updateWhileOn(ctx context.Context, on func() bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-n.wake:
				if !on() {
					cancel()
					return
				}
			}
		}
	}()
	return n.update(ctx)
}

func (n *names) set(s *steamNames) {
	n.mu.Lock()
	n.steam = s
	n.mu.Unlock()
}

func (n *names) release() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.steam == nil {
		return ""
	}
	return n.steam.Release
}

// load reads the map saved by an earlier run, if there is one.
func (n *names) load() {
	if n.file == "" {
		return
	}
	b, err := os.ReadFile(n.file)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var s steamNames
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		n.log.Warn("game names: ignoring saved Steam names", "file", n.file, "err", err)
		return
	}
	n.set(&s)
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

// update downloads the newest Steam map unless it is already loaded.
func (n *names) update(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var rel struct {
		Tag    string         `json:"tag_name"`
		Assets []releaseAsset `json:"assets"`
	}
	body, err := n.get(ctx, steamRelease)
	if err != nil {
		return err
	}
	err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&rel)
	body.Close()
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if rel.Tag != "" && rel.Tag == n.release() {
		return nil
	}
	i := slices.IndexFunc(rel.Assets, func(a releaseAsset) bool { return a.Name == steamAsset })
	if i < 0 {
		return fmt.Errorf("release %s has no %s", rel.Tag, steamAsset)
	}
	asset := rel.Assets[i]
	want, ok := strings.CutPrefix(asset.Digest, "sha256:")
	if !ok {
		return fmt.Errorf("release %s gives no SHA-256 for %s", rel.Tag, steamAsset)
	}

	body, err = n.get(ctx, asset.URL)
	if err != nil {
		return err
	}
	defer body.Close()
	h := sha256.New()
	r := io.TeeReader(io.LimitReader(body, maxSteamMap), h)
	s, err := parseSteamMap(r)
	if err != nil {
		return fmt.Errorf("%s: %w", steamAsset, err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("%s does not match its SHA-256", steamAsset)
	}
	if len(s.Depots) < minSteamDepots {
		return fmt.Errorf("%s names only %d depots", steamAsset, len(s.Depots))
	}
	s.Release = rel.Tag
	n.set(s)
	n.log.Info("game names: Steam names updated", "release", rel.Tag, "depots", len(s.Depots), "games", len(s.Apps))
	if n.file == "" {
		return nil
	}
	b, err := json.Marshal(s)
	if err == nil {
		err = writeFileAtomic(n.file, b)
	}
	if err != nil {
		n.log.Warn("game names: Steam names not saved", "file", n.file, "err", err)
	}
	return nil
}

func (n *names) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "CacheParty")
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// parseSteamMap reads a lancache-pics map one depot at a time, so the whole
// file is never held in memory. A depot is kept only when its record names
// the app that owns it.
func parseSteamMap(r io.Reader) (*steamNames, error) {
	s := &steamNames{Depots: map[uint32]uint32{}, Apps: map[uint32]string{}}
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if key != "depotMappings" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		if err := expectDelim(dec, '{'); err != nil {
			return nil, err
		}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, err
			}
			var e struct {
				Owner int64    `json:"ownerId"`
				Apps  []int64  `json:"appIds"`
				Names []string `json:"appNames"`
			}
			if json.Unmarshal(raw, &e) != nil {
				continue
			}
			depot, err := strconv.ParseUint(key.(string), 10, 32)
			i := slices.Index(e.Apps, e.Owner)
			if err != nil || depot == 0 || e.Owner <= 0 || e.Owner > 1<<32-1 || i < 0 || i >= len(e.Names) {
				continue
			}
			name := strings.TrimSpace(e.Names[i])
			if name == "" || len(name) > maxNameLen || !utf8.ValidString(name) {
				continue
			}
			s.Depots[uint32(depot)] = uint32(e.Owner)
			s.Apps[uint32(e.Owner)] = name
		}
		if err := expectDelim(dec, '}'); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != want {
		return fmt.Errorf("expected %v, got %v", want, tok)
	}
	return nil
}

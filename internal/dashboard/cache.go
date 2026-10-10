package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"cacheparty/internal/store"
)

// Cache repair, for the dashboard and the cacheparty verify and purge commands.
// Like settings changes, these need DASHBOARD_PASSWORD.

type verifyJSON struct {
	DryRun  bool  `json:"dryRun"`
	Files   int   `json:"files"`
	Orphans int   `json:"orphans"`
	Missing int   `json:"missing"`
	Corrupt int   `json:"corrupt"`
	Freed   int64 `json:"freed"`
}

// verifyCache takes {"dryRun": bool} and checks every cached chunk's file
// against the index, answering when the check is done. It can take minutes
// on a large cache; a client that gives up stops it.
func (d *Dashboard) verifyCache(w http.ResponseWriter, r *http.Request) {
	if !d.mayChange(w, r) {
		return
	}
	var body struct {
		DryRun bool `json:"dryRun"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	res, err := d.cfg.Verify(r.Context(), body.DryRun)
	switch {
	case errors.Is(err, store.ErrScrubRunning):
		writeError(w, http.StatusConflict, "a cache check is already running; try again later")
		return
	case err != nil:
		if r.Context().Err() == nil {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	d.cfg.Log.Info("cache verified from dashboard", "remote", r.RemoteAddr, "dry_run", body.DryRun,
		"files", res.Files, "orphan_files", res.Orphans, "missing_files", res.Missing,
		"corrupt_files", res.Corrupt, "freed_bytes", res.Freed)
	writeJSON(w, http.StatusOK, verifyJSON{body.DryRun, res.Files, res.Orphans, res.Missing, res.Corrupt, res.Freed})
}

// readJSON decodes a small request body into v, answering the request if it
// is not valid. An empty body leaves v as it is.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

type purgeJSON struct {
	App       string `json:"app"`
	Confirmed bool   `json:"confirmed"` // false: a preview; nothing was removed
	Objects   int    `json:"objects"`
	Chunks    int    `json:"chunks"`
	Bytes     int64  `json:"bytes"`
}

// purgeApp takes {"service", "kind", "id", "confirm"}: what the services
// API lists. Without "confirm": true it only reports what would be removed,
// so a client can show that before asking to go ahead.
func (d *Dashboard) purgeApp(w http.ResponseWriter, r *http.Request) {
	if !d.mayChange(w, r) {
		return
	}
	var body struct {
		Service string `json:"service"`
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	a := app{body.Kind, body.ID}
	if body.Service == "" || a.Kind == "" || a.ID == "" || a == otherApp ||
		strings.Contains(body.Service+a.Kind+a.ID, "/") {
		writeError(w, http.StatusBadRequest, "service, kind and id are required, as /api/services lists them")
		return
	}
	tag := a.tag(body.Service)
	res := d.cfg.Purge(tag, !body.Confirm)
	if body.Confirm {
		d.cfg.Log.Info("cache purged from dashboard", "remote", r.RemoteAddr, "app", tag,
			"objects", res.Objects, "chunks", res.Chunks, "freed_bytes", res.Bytes)
	}
	writeJSON(w, http.StatusOK, purgeJSON{tag, body.Confirm, res.Objects, res.Chunks, res.Bytes})
}

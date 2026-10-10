package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"cacheparty/internal/config"
)

// The cache-repair commands ask the running CacheParty to do the work, through
// the dashboard's API: the index lives in that process's memory, and bbolt
// lets only one process open it. So they need METRICS_ADDR on and
// DASHBOARD_PASSWORD set, as in the container they run in
// (docker exec <container> /cacheparty verify).

// adminClient talks to the dashboard API of a running CacheParty.
type adminClient struct {
	base     string // http://host:port
	password string
}

// newAdminClient reads the dashboard's address (METRICS_ADDR, or addr if
// not empty) and DASHBOARD_PASSWORD from the environment.
func newAdminClient(addr string) (*adminClient, error) {
	if addr == "" {
		addr = config.Raw(os.Getenv, "METRICS_ADDR")
	}
	if strings.EqualFold(addr, "off") {
		return nil, errors.New("METRICS_ADDR is off; these commands use the dashboard API")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("dashboard address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	pw := os.Getenv("DASHBOARD_PASSWORD")
	if pw == "" {
		return nil, errors.New("DASHBOARD_PASSWORD is not set; it is needed to change the cache")
	}
	return &adminClient{base: "http://" + net.JoinHostPort(host, port), password: pw}, nil
}

// post sends body as JSON to path and decodes the answer into out. There is
// no timeout: checking a large cache takes minutes.
func (c *adminClient) post(path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("is CacheParty running? %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}

// verifyCmd is "cacheparty verify [-dry-run]".
func verifyCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "report problems without repairing them")
	addr := fs.String("addr", "", "dashboard `host:port` (default from METRICS_ADDR)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: cacheparty verify [-dry-run] [-addr host:port]")
		fmt.Fprintln(fs.Output(), "\nChecks every cached chunk's file against the index: removes files the index")
		fmt.Fprintln(fs.Output(), "does not know, forgets chunks whose file is gone, and removes files of the")
		fmt.Fprintln(fs.Output(), "wrong size. Contents are not read, so it is quick even for a large cache.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return errors.New("unexpected arguments")
	}
	c, err := newAdminClient(*addr)
	if err != nil {
		return err
	}
	var res struct {
		Files, Orphans, Missing, Corrupt int
		Freed                            int64
	}
	if err := c.post("/api/cache/verify", map[string]bool{"dryRun": *dry}, &res); err != nil {
		return err
	}
	verb := "removed"
	if *dry {
		verb = "would remove"
	}
	fmt.Fprintf(stdout, "checked %d chunk files\n", res.Files)
	fmt.Fprintf(stdout, "orphan files (not in the index): %d\n", res.Orphans)
	fmt.Fprintf(stdout, "missing files (in the index, not on disk): %d\n", res.Missing)
	fmt.Fprintf(stdout, "corrupt files (wrong size): %d\n", res.Corrupt)
	fmt.Fprintf(stdout, "%s %s\n", verb, fmtBytes(res.Freed))
	if res.Missing+res.Corrupt > 0 {
		fmt.Fprintln(stdout, "error.log names each missing and corrupt chunk")
	}
	return nil
}

// fmtBytes formats n with a binary unit, as the dashboard does.
func fmtBytes(n int64) string {
	const units = "KMGTPE"
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n)/1024, 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %ciB", v, units[i])
}

// purgeCmd is "cacheparty purge [-yes] <service>/<kind>/<id>". It shows what
// would be removed and asks before removing it, unless -yes.
func purgeCmd(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "remove without asking")
	addr := fs.String("addr", "", "dashboard `host:port` (default from METRICS_ADDR)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: cacheparty purge [-yes] [-addr host:port] <service>/<kind>/<id>")
		fmt.Fprintln(fs.Output(), "\nRemoves every cached chunk of one game, named as the dashboard lists it:")
		fmt.Fprintln(fs.Output(), "steam/depot/990081, blizzard/product/wow, epicgames/app/Fortnite or")
		fmt.Fprintln(fs.Output(), "riot/host/lol.dyn.riotcdn.net.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	parts := strings.Split(fs.Arg(0), "/")
	if fs.NArg() != 1 || len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		fs.Usage()
		return errors.New("want one <service>/<kind>/<id>")
	}
	c, err := newAdminClient(*addr)
	if err != nil {
		return err
	}
	type result struct {
		Objects, Chunks int
		Bytes           int64
	}
	req := map[string]any{"service": parts[0], "kind": parts[1], "id": parts[2], "confirm": false}
	var preview result
	if err := c.post("/api/cache/purge", req, &preview); err != nil {
		return err
	}
	if preview.Chunks == 0 {
		fmt.Fprintf(stdout, "nothing cached for %s\n", fs.Arg(0))
		return nil
	}
	fmt.Fprintf(stdout, "%s: %d chunks in %d files, %s\n", fs.Arg(0), preview.Chunks, preview.Objects, fmtBytes(preview.Bytes))
	if !*yes {
		fmt.Fprint(stdout, "Remove them? [y/N] ")
		var answer string
		fmt.Fscanln(stdin, &answer)
		if a := strings.ToLower(answer); a != "y" && a != "yes" {
			fmt.Fprintln(stdout)
			return errors.New("not confirmed; nothing removed (-yes skips the question)")
		}
	}
	req["confirm"] = true
	var done result
	if err := c.post("/api/cache/purge", req, &done); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed %d chunks, freed %s\n", done.Chunks, fmtBytes(done.Bytes))
	return nil
}

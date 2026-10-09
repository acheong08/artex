// Package selfupdate implements ARTEX's one-click in-app update: fetch a new binary
// from GitHub Releases, verify and stage it, then atomically replace the binary on next startup.
//
// Responsibility split (see start.sh / start.bat):
//
//	Startup script = simple supervisor loop; restarts the process based on its exit code.
//	This package  = all error-prone logic (download / SHA256 verification / smoke test / replacement / rollback).
//
// Replacement is implemented in Go rather than scripts because SHA256 verification
// and smoke tests would need separate sh and bat implementations (sha256sum / shasum /
// certutil), and this is the part that must not fail. If an unusable binary were
// installed, the supervisor would keep restarting it and the user would have to recover
// the machine manually.
//
// A complete update requires three process starts:
//
//	1. Old server receives /api/update/apply -> download/verify -> stage artex.new -> exit 75.
//	2. Script restarts old version -> Bootstrap finds artex.new -> verify/smoke-test -> replace -> exit 75.
//	3. Script restarts the new version -> Bootstrap records an attempt -> clear marker after successful startup.
//
// Any failure returns to the old version: at step 2, discard a staged binary that
// fails validation and continue running the old version; at step 3, automatically
// restore artex.old if the new version fails to survive three startup attempts.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the exit code (EX_TEMPFAIL) meaning "ask the supervisor to restart
// me." The startup script restarts immediately without crash backoff. Zero means
// normal user shutdown (the script exits its loop); other codes mean a crash.
const ExitRestart = 75

// maxAttempts is the number of startup attempts allowed after replacement. Each start
// of the new version increments the count; surviving settleDelay clears the marker.
// Reaching maxAttempts crashes means the new version cannot start, so roll back automatically.
const maxAttempts = 3

// Paths contains all files involved in an update, located in the **executable's
// directory**. Deliberately avoid CWD: a service's working directory may be / or any
// arbitrary path, which could stage files elsewhere and break replacement.
type Paths struct {
	Dir     string // Directory containing the executable.
	Current string // Current binary: artex / artex.exe.
	New     string // Staged new version: artex.new / artex.new.exe.
	Sum     string // New-version SHA256 (hex): artex.new.sha256 / artex.new.exe.sha256.
	Old     string // Pre-update backup: artex.old / artex.old.exe.
	Marker  string // Update-state marker: artex.upgrade.json.
}

// ResolvePaths derives all update paths from the current executable.
//
// On Windows, .new/.old must also retain the .exe suffix or smoke testing and execution
// after replacement will fail. Strip the extension first and then append suffixes so
// names are consistent across platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows; usually empty on Unix.
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records update progress and triggers automatic rollback if the new version fails to start.
type marker struct {
	From     string `json:"from"`     // Version before update.
	To       string `json:"to"`       // Target version.
	Attempts int    `json:"attempts"` // Number of startup attempts since replacement.
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged removes staged files after a successful replacement, failed validation,
// or user cancellation so artex.new is not retried on the next startup.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version strings and returns -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one version is not comparable (e.g. a local "dev" build or
// git describe output such as "0.3.7-2-gabc1234-dirty"). Callers should disable
// one-click updates in this case, or an update to a stable release could overwrite
// uncommitted development changes.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses a version string in "v0.3.7" / "0.3.7" form into [3]int.
//
// Accept only clean three-part versions. For non-tag builds, build.sh uses git describe
// to produce versions like "0.3.7-2-gabc1234"; treat these as incomparable rather than
// as 0.3.7, or a development build could be considered up to date or overwritten by a release.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process is running in a container. Updates in Docker
// modify the container's writable layer; `docker compose up -d` recreates the
// container with the image's version. This is expected (the user is pulling a new
// image), but the frontend should explain this accurately.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}

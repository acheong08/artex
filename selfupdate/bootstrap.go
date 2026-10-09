package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv tells subprocesses started by the smoke test to skip Bootstrap.
//
// Strictly speaking, this is not required: os.Executable() in the subprocess points
// to artex.new, so all derived paths have a .new prefix and cannot touch real update
// files. But relying on this coincidence is fragile; an explicit short circuit is
// clearer and avoids an unnecessary disk probe in the subprocess.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is an instruction from Bootstrap to main.
type Action int

const (
	// Continue: start the server normally.
	Continue Action = iota
	// Restart: exit immediately with ExitRestart so the supervisor restarts the process.
	Restart
)

// State describes the update status at startup so /api/update/check can tell the
// frontend whether the previous update succeeded or was rolled back.
type State struct {
	Pending     bool   // New binary has not yet been confirmed stable.
	RolledBack  bool   // Automatic rollback was just performed during this startup.
	FailedStage bool   // Staged binary failed validation/smoke test and was discarded.
	Detail      string // One-sentence explanation for the user.
}

// Bootstrap runs at the very beginning of main and must be called before opening a
// listener or database.
//
// There are three possible states:
//
//	1. Staged artex.new exists -> validate and smoke-test; replace and restart on success, discard and run old version on failure.
//	2. Only marker file remains -> replacement just completed; record an attempt and roll back after repeated failures.
//	3. Neither exists -> start normally.
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] bootstrap skipped: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged handles the staged-binary case: replace on successful validation, discard on failure.
//
// This is the only place in the update flow that replaces the executable, and the
// final safeguard. The smoke test catches corrupt downloads, wrong architecture, and
// missing dynamic libraries. If an unusable binary passes, the supervisor will keep
// restarting it without giving Go code a chance to run, so automatic rollback cannot work.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] staged update failed verification and was discarded; continuing with current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "Update verification failed and the staged version was discarded: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] failed to replace binary; continuing with current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "Failed to replace binary: " + err.Error()}
	}

	// Replacement succeeded. Keep the marker so the next startup (now running the new
	// version) can confirm that it is stable.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to write update marker (automatic rollback is unavailable): %v", err)
	}
	log.Printf("[update] updated to %s; exiting to restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback handles startup after replacement: count attempts and roll back
// to the old version when the limit is exceeded.
//
// Attempts are counted only after Go code starts, covering failures where the binary
// executes but crashes during initialization (incompatible config, occupied port, DB
// migration failure). The pre-replacement smoke test catches binaries that cannot
// execute at all; together these provide full coverage.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// If rollback failed, do not restart again or the process may loop forever.
			// Clear the marker and start in the current state; if startup fails, the user
			// can at least see the cause in the logs.
			log.Printf("[update] new version failed to start %d consecutive times and rollback failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "The new version failed to start and rollback failed: " + err.Error()}
		}
		log.Printf("[update] new version failed to start %d consecutive times; rolled back to %s, exiting to restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("The new version failed to start; rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to update update marker: %v", err)
	}
	log.Printf("[update] starting new version (attempt %d/%d); update will be confirmed after it runs stably",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms the new version is running stably and clears the update marker.
//
// Called by main after the HTTP listener has been running for a delay. Otherwise the
// marker remains, and subsequent startups keep counting attempts until rollback occurs.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // Not a post-update startup; nothing to do.
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] failed to clear update marker: %v", err)
		return
	}
	log.Printf("[update] new version is stable; update complete (previous version retained at %s)", p.Old)
}

// SettleDelay is the runtime required to consider the new version stable.
const SettleDelay = 30 * time.Second

// verifyStaged validates a staged binary: check its SHA256, then actually launch it once.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("calculate checksum: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 mismatch (download is corrupted or has been tampered with)")
	}
	return smokeTest(p.New)
}

// smokeTest launches the new binary with -h to verify that it runs on this system.
// This catches truncated downloads, incorrect architecture (exec format error), and missing dependencies.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("set executable permissions: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("smoke test timed out (new binary did not respond)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap replaces the current binary with the staged version.
//
// Unix and Windows both allow renaming a running executable (Windows forbids deleting
// or overwriting it, but not renaming), so this needs neither platform-specific code
// nor stopping the current process first.
func swap(p Paths) error {
	// Windows rename does not overwrite an existing destination, so remove the .old file from a previous update first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("back up current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// Replacement failed after moving the current version; restore it or the next startup will have no executable.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("failed to install new version (%v) and restore current version: %w", err, rerr)
		}
		return fmt.Errorf("install new version: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback restores the old version saved by swap.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("no rollback backup available at %s: %w", p.Old, err)
	}
	// Move the unusable new version to .failed for troubleshooting rather than deleting it.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("move failed version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("restore previous version: %w", err)
	}
	return nil
}

// Rollback implements /api/update/rollback: explicitly return to the previous version.
// It only replaces the binary; the supervisor handles restart (the caller exits with ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("no previous version is available to roll back to (" + p.Old + " does not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("previous version cannot run; refusing to roll back: %w", err)
	}
	// Swap the current and backup versions so rollback can itself be reversed.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("move current version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("install previous version: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] failed to clean up backup after rollback (does not affect operation): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether a previous version is available for rollback, so the frontend
// can decide whether to show the rollback button.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown version"
	}
	return s
}

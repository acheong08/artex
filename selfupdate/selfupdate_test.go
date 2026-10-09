package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths creates an isolated update directory. Do not call ResolvePaths(), which
// would point at the test binary and rename the go test executable.
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin writes an executable shell script to stand in for artex. smokeTest only
// invokes it with -h and checks the exit code, so a script is sufficient and much
// faster than compiling a real binary.
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary %s: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage prepares bin as an already-staged update by writing artex.new and its checksum.
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("calculate checksum: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("write checksum: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script and cannot run on Windows")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh strips v; tags include it, so both forms must work.
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // Compare numerically, not lexicographically.
		{"1.0.0", "0.99.99", 1, true},
		// Development builds must be incomparable, or a release could overwrite uncommitted changes.
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v, expected %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d, expected %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// Key invariant: all update files share the executable's directory. Using CWD would
	// break replacement in service deployments, where the working directory may be /.
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s is not beside the executable: %s (expected %s)", name, path, p.Dir)
		}
	}
	// On Windows, .new/.old must keep the .exe suffix or smoke tests and execution after replacement will fail.
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows .new/.old files must end in .exe: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// Modify the file after writing its checksum to simulate corruption or tampering after download.
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("expected SHA256 mismatch to be rejected, but it passed")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // Executable, but returns a nonzero exit code.

	if err := verifyStaged(p); err == nil {
		t.Fatal("expected smoke-test failure to be rejected, but it passed")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("expected Restart, got %v", action)
	}
	if !st.Pending {
		t.Error("state after replacement should be Pending")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex should have been replaced with the new version")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("old version should be backed up to artex.old")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("artex.new should be gone after replacement")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("checksum file should be removed after replacement")
	}
	// Keep the marker so the next startup (running the new version) can count attempts and roll back if needed.
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("update marker should remain after replacement")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // Corrupt the checksum.

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("expected Continue after validation failure, got %v", action)
	}
	if !st.FailedStage {
		t.Error("state should be marked FailedStage")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("validation failure must never change the current version")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("failed staged binary should be removed or the next startup will try it again")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // Backup left by the previous update.
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("should replace with v3")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("backup should be updated to the just-replaced v2")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// Count the first maxAttempts startups, giving the new version a chance to stabilize.
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("attempt %d: expected Continue, got %v", i, action)
		}
		if !st.Pending {
			t.Errorf("attempt %d: state should be Pending", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("after attempt %d, attempts=%d (ok=%v), expected %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// One more crash exceeds the limit and automatically restores the old version.
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("expected Restart after exceeding the attempt limit, got %v", action)
	}
	if !st.RolledBack {
		t.Error("state should be marked RolledBack")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("should have rolled back to the old version")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("marker should be cleared after rollback or it will roll back repeatedly")
	}
	// Keep the unusable version for troubleshooting rather than deleting it.
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("failed version should be kept as .failed for troubleshooting")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback() uses ResolvePaths(); test the underlying swap semantics directly here.
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("current version should be v1 after rollback")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("backup should be v2 after rollback so it can be restored again")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum separates fields with two spaces; shasum -a 256 prefixes the filename with * in binary mode.
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // Exactly two fields, but the first is not a digest.
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // Digest is the wrong length.

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("incorrectly parsed Linux entry: %v", out)
	}
	// Normalize digests to lowercase to avoid case-sensitive mismatches.
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("incorrect Windows entry (* prefix should be stripped and digest lowercased): %q", got)
	}
	if len(out) != 2 {
		t.Errorf("expected blank, non-digest, and wrong-length lines to be ignored, got %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the package basename is artex.exe on Windows; this case uses Unix naming")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// A real release package contains artex-<version>-<os>-<arch>/artex and other unrelated files.
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("extracted file is not the artex executable: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("extracted binary must be executable")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("missing executable in package should return an error")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // Not HTTPS.
		"https://evil.com/artex.zip",    // Host is not allowlisted.
		"https://github.com.evil.com/x", // Spoofed suffix.
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) should reject the URL", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // Domain matching is case-insensitive.
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) should allow the URL, but returned an error: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// package_binary in build.sh uses artex-<version>-<os>-<arch>.zip without the v
	// prefix. A one-character mismatch here would make one-click updates fail to find
	// assets on every platform.
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("update marker must be cleared after stability is confirmed")
	}
	// Once the marker is gone, normal restarts should no longer count attempts or trigger rollback.
	if _, ok := readMarker(p.Marker); ok {
		t.Error("reading the marker should fail")
	}
	// Keep the backup so the user can roll back manually.
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("the previous-version backup should remain after stability is confirmed")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // Normal startup path: should not panic or modify files.
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("settle should not affect files when no marker exists")
	}
}

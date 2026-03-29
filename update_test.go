package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReleaseAssetName(t *testing.T) {
	tests := []struct {
		goos   string
		goarch string
		want   string
	}{
		{goos: "linux", goarch: "amd64", want: "deploy_v1.2.3_linux_amd64"},
		{goos: "darwin", goarch: "amd64", want: "deploy_v1.2.3_darwin_amd64"},
		{goos: "darwin", goarch: "arm64", want: "deploy_v1.2.3_darwin_arm64"},
		{goos: "windows", goarch: "amd64", want: "deploy_v1.2.3_windows_amd64.exe"},
	}

	for _, tt := range tests {
		got, err := releaseAssetName("v1.2.3", tt.goos, tt.goarch)
		if err != nil {
			t.Fatalf("releaseAssetName returned error for %s/%s: %v", tt.goos, tt.goarch, err)
		}
		if got != tt.want {
			t.Fatalf("unexpected asset name for %s/%s: got %q want %q", tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestResolveUpdateTargetPrefersCurrentUserScopedBinary(t *testing.T) {
	env := map[string]string{"HOME": "/home/alice"}
	currentExecutable := "/home/alice/.local/bin/deploy"

	target, err := resolveUpdateTarget("linux", env, "/usr/local/bin", currentExecutable, func(path string) bool {
		return normalizePathForOS("linux", path) == normalizePathForOS("linux", "/home/alice/.local/bin")
	})
	if err != nil {
		t.Fatalf("resolveUpdateTarget returned error: %v", err)
	}
	if target.path != currentExecutable {
		t.Fatalf("expected current executable target %q, got %q", currentExecutable, target.path)
	}
	if target.pathInstruction != `export PATH="$HOME/.local/bin:$PATH"` {
		t.Fatalf("unexpected PATH instruction %q", target.pathInstruction)
	}
}

func TestRunSelfUpdateInstallsDirectBinary(t *testing.T) {
	restore := overrideUpdateGlobals()
	defer restore()

	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	cacheDir := filepath.Join(tmpDir, "cache")
	currentDir := filepath.Join(homeDir, ".local", "bin")
	if err := os.MkdirAll(currentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	currentExecutable := filepath.Join(currentDir, executableName(runtime.GOOS))
	if err := os.WriteFile(currentExecutable, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	nextVersion := "v1.1.0"
	assetName, err := releaseAssetName(nextVersion, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}

	newBinary := []byte("new-binary")
	sum := sha256.Sum256(newBinary)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			release := githubRelease{
				TagName: nextVersion,
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: server.URL + "/assets/" + assetName},
					{Name: checksumsAssetName, BrowserDownloadURL: server.URL + "/assets/" + checksumsAssetName},
				},
			}
			if err := json.NewEncoder(w).Encode(release); err != nil {
				t.Fatalf("failed to encode release: %v", err)
			}
		case "/assets/" + assetName:
			_, _ = w.Write(newBinary)
		case "/assets/" + checksumsAssetName:
			_, _ = w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	updateLatestReleaseURL = server.URL + "/latest"
	updateHTTPClient = server.Client()
	updateEnvironment = func() map[string]string {
		env := map[string]string{
			"HOME":        homeDir,
			"USERPROFILE": homeDir,
		}
		if runtime.GOOS == "windows" {
			env["LOCALAPPDATA"] = filepath.Join(homeDir, "AppData", "Local")
		}
		return env
	}
	updateExecutable = func() (string, error) { return currentExecutable, nil }
	updatePathValue = func() string { return currentDir }
	updateCacheRoot = func() (string, error) { return cacheDir, nil }
	updateCanWriteDir = func(string) bool { return true }
	updateClock = func() time.Time { return time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC) }

	previousVersion := version
	version = "v1.0.0"
	defer func() {
		version = previousVersion
	}()

	var stdout strings.Builder
	application := &app{
		reader: bufio.NewReader(strings.NewReader("")),
		stdout: &stdout,
		stderr: &strings.Builder{},
	}

	if err := application.run([]string{"--update"}); err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	installedBinary, err := os.ReadFile(currentExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if string(installedBinary) != string(newBinary) {
		t.Fatalf("unexpected installed binary: got %q want %q", string(installedBinary), string(newBinary))
	}
	if !strings.Contains(stdout.String(), "Updated deploy to") {
		t.Fatalf("expected update output, got %q", stdout.String())
	}
}

func TestCheckForStartupUpdateUsesCachedResultAndStoresDecline(t *testing.T) {
	restore := overrideUpdateGlobals()
	defer restore()

	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	updateCacheRoot = func() (string, error) { return cacheDir, nil }
	updateClock = func() time.Time { return time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC) }

	previousVersion := version
	version = "v1.0.0"
	defer func() {
		version = previousVersion
	}()

	updater := newUpdater(&app{
		reader: bufio.NewReader(strings.NewReader("2\n")),
		stdout: &strings.Builder{},
		stderr: &strings.Builder{},
	})

	if err := updater.saveCache(updateCacheState{
		CheckedAt:      updateClock(),
		CurrentVersion: version,
		LatestVersion:  "v1.1.0",
	}); err != nil {
		t.Fatalf("saveCache returned error: %v", err)
	}

	handled, err := updater.checkForStartupUpdate()
	if err != nil {
		t.Fatalf("checkForStartupUpdate returned error: %v", err)
	}
	if handled {
		t.Fatal("did not expect cached decline flow to exit the app")
	}

	state, hasCache, err := updater.loadCache()
	if err != nil {
		t.Fatalf("loadCache returned error: %v", err)
	}
	if !hasCache {
		t.Fatal("expected update cache to exist")
	}
	if state.DeclinedVersion != "v1.1.0" {
		t.Fatalf("expected declined version to be recorded, got %q", state.DeclinedVersion)
	}
}

func overrideUpdateGlobals() func() {
	previousReleaseURL := updateLatestReleaseURL
	previousClock := updateClock
	previousCacheRoot := updateCacheRoot
	previousClient := updateHTTPClient
	previousEnvironment := updateEnvironment
	previousExecutable := updateExecutable
	previousPathValue := updatePathValue
	previousCanWriteDir := updateCanWriteDir

	return func() {
		updateLatestReleaseURL = previousReleaseURL
		updateClock = previousClock
		updateCacheRoot = previousCacheRoot
		updateHTTPClient = previousClient
		updateEnvironment = previousEnvironment
		updateExecutable = previousExecutable
		updatePathValue = previousPathValue
		updateCanWriteDir = previousCanWriteDir
	}
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	updateCheckTTL                = 24 * time.Hour
	updateRequestTimeout          = 20 * time.Second
	updateMaxAttempts             = 3
	updateRetryDelay              = 2 * time.Second
	checksumsAssetName            = "checksums.txt"
	internalWindowsReplaceCommand = "__replace-binary"
)

var (
	updateLatestReleaseURL = "https://api.github.com/repos/allwinpower/deploy/releases/latest"
	updateClock            = time.Now
	updateCacheRoot        = os.UserCacheDir
	updateHTTPClient       = &http.Client{Timeout: updateRequestTimeout}
	updateEnvironment      = currentEnvironment
	updateExecutable       = currentExecutablePath
	updatePathValue        = func() string { return os.Getenv("PATH") }
	updateCanWriteDir      = isDirWritable
	updateSleep            = time.Sleep
)

type githubRelease struct {
	TagName    string         `json:"tag_name"`
	HTMLURL    string         `json:"html_url"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type updateCacheState struct {
	CheckedAt       time.Time `json:"checked_at"`
	CurrentVersion  string    `json:"current_version"`
	LatestVersion   string    `json:"latest_version,omitempty"`
	DeclinedVersion string    `json:"declined_version,omitempty"`
}

type semver struct {
	major int
	minor int
	patch int
}

type updater struct {
	app         *app
	client      *http.Client
	now         func() time.Time
	cacheRoot   func() (string, error)
	environment map[string]string
	pathValue   string
	canWriteDir func(string) bool
}

func newUpdater(a *app) *updater {
	return &updater{
		app:         a,
		client:      updateHTTPClient,
		now:         updateClock,
		cacheRoot:   updateCacheRoot,
		environment: updateEnvironment(),
		pathValue:   updatePathValue(),
		canWriteDir: updateCanWriteDir,
	}
}

func (a *app) checkForStartupUpdate() (bool, error) {
	return newUpdater(a).checkForStartupUpdate()
}

func (a *app) runSelfUpdate() error {
	return newUpdater(a).runSelfUpdate()
}

func (u *updater) checkForStartupUpdate() (bool, error) {
	if !isTaggedReleaseVersion(version) {
		return false, nil
	}

	cache, hasCache, err := u.loadCache()
	if err == nil && hasCache && cache.CurrentVersion == version && u.now().Sub(cache.CheckedAt) < updateCheckTTL {
		newer, compareErr := isNewerVersion(cache.LatestVersion, version)
		if compareErr != nil || !newer || cache.DeclinedVersion == cache.LatestVersion {
			return false, nil
		}
		return u.promptForUpdate(cache.LatestVersion)
	}

	release, err := u.fetchLatestRelease()
	if err != nil {
		return false, nil
	}

	cache = updateCacheState{
		CheckedAt:      u.now(),
		CurrentVersion: version,
	}
	if newer, compareErr := isNewerVersion(release.TagName, version); compareErr == nil && newer {
		cache.LatestVersion = release.TagName
	}
	_ = u.saveCache(cache)

	if cache.LatestVersion == "" {
		return false, nil
	}
	return u.promptForUpdate(cache.LatestVersion)
}

func (u *updater) promptForUpdate(latest string) (bool, error) {
	choice, err := u.app.promptSelect(
		"Update Available",
		fmt.Sprintf("A newer version of deploy is available (%s -> %s). Update now?", version, latest),
		[]menuOption{
			{label: "Update now"},
			{label: "Later"},
		},
	)
	if err != nil {
		if errors.Is(err, errSelectionCancelled) {
			err = nil
		} else {
			return false, nil
		}
	}

	if choice != "Update now" {
		_ = u.saveCache(updateCacheState{
			CheckedAt:       u.now(),
			CurrentVersion:  version,
			LatestVersion:   latest,
			DeclinedVersion: latest,
		})
		return false, nil
	}

	if err := u.applyLatestRelease(); err != nil {
		u.app.msgBox("Update Failed", err.Error())
		return false, nil
	}

	return true, nil
}

func (u *updater) runSelfUpdate() error {
	if !isTaggedReleaseVersion(version) {
		return errors.New("self-update requires a tagged release build")
	}

	release, err := u.fetchLatestRelease()
	if err != nil {
		return err
	}

	newer, err := isNewerVersion(release.TagName, version)
	if err != nil {
		return err
	}
	if !newer {
		fmt.Fprintf(u.app.stdout, "%s is already up to date\n", versionString())
		return nil
	}

	if err := u.applyRelease(release); err != nil {
		return err
	}

	_ = u.saveCache(updateCacheState{
		CheckedAt:      u.now(),
		CurrentVersion: version,
		LatestVersion:  release.TagName,
	})

	return nil
}

func (u *updater) applyLatestRelease() error {
	release, err := u.fetchLatestRelease()
	if err != nil {
		return err
	}

	newer, err := isNewerVersion(release.TagName, version)
	if err != nil {
		return err
	}
	if !newer {
		fmt.Fprintf(u.app.stdout, "%s is already up to date\n", versionString())
		return nil
	}

	if err := u.applyRelease(release); err != nil {
		return err
	}

	_ = u.saveCache(updateCacheState{
		CheckedAt:      u.now(),
		CurrentVersion: version,
		LatestVersion:  release.TagName,
	})

	return nil
}

func (u *updater) applyRelease(release githubRelease) error {
	asset, checksumAsset, err := selectReleaseAssets(release, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	checksums, err := u.downloadText(checksumAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("failed to download checksums: %w", err)
	}

	downloadPath, err := u.downloadAssetToTemp(asset)
	if err != nil {
		return err
	}
	cleanupDownload := true
	defer func() {
		if cleanupDownload {
			_ = os.Remove(downloadPath)
		}
	}()

	if err := verifyChecksumFile(downloadPath, asset.Name, checksums); err != nil {
		return err
	}

	currentExecutable, err := updateExecutable()
	if err != nil {
		return err
	}

	target, err := resolveUpdateTarget(runtime.GOOS, u.environment, u.pathValue, currentExecutable, u.canWriteDir)
	if err != nil {
		return err
	}

	if runtime.GOOS == "windows" && normalizePathForOS(runtime.GOOS, target.path) == normalizePathForOS(runtime.GOOS, currentExecutable) {
		if err := spawnWindowsReplaceHelper(currentExecutable, downloadPath, target.path); err != nil {
			return err
		}
		cleanupDownload = false
		fmt.Fprintf(u.app.stdout, "Update scheduled for %s. Restart deploy in a moment.\n", target.path)
		if target.pathInstruction != "" {
			fmt.Fprintf(u.app.stdout, "Add it to PATH with:\n%s\n", target.pathInstruction)
		}
		return nil
	}

	if _, err := installExecutable(downloadPath, target.path, runtime.GOOS); err != nil {
		return err
	}
	fmt.Fprintf(u.app.stdout, "Updated deploy to %s\n", target.path)
	if target.pathInstruction != "" {
		fmt.Fprintf(u.app.stdout, "Add it to PATH with:\n%s\n", target.pathInstruction)
	}

	return nil
}

func resolveUpdateTarget(goos string, env map[string]string, pathValue, currentExecutable string, canWriteDir func(string) bool) (installTarget, error) {
	currentExecutable = strings.TrimSpace(currentExecutable)
	if currentExecutable != "" {
		currentDir := filepath.Dir(currentExecutable)
		if isUserScopedPath(goos, currentDir, env) && canWriteDir(currentDir) {
			target := installTarget{
				dir:  currentDir,
				path: currentExecutable,
			}
			if !pathContainsDir(goos, pathValue, currentDir) {
				target.pathInstruction = pathInstruction(goos, env, currentDir)
			}
			return target, nil
		}
	}

	return resolveInstallTarget(goos, env, pathValue, canWriteDir)
}

func currentExecutablePath() (string, error) {
	sourcePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to resolve current executable: %w", err)
	}

	resolvedSource, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sourcePath, nil
		}
		return "", fmt.Errorf("failed to resolve executable symlink: %w", err)
	}

	return resolvedSource, nil
}

func (u *updater) fetchLatestRelease() (githubRelease, error) {
	body, err := u.getURLBytes(updateLatestReleaseURL, "release metadata", "application/vnd.github+json")
	if err != nil {
		return githubRelease{}, err
	}
	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return githubRelease{}, err
	}
	if release.Prerelease {
		return githubRelease{}, errors.New("latest release is marked as a prerelease")
	}
	if !isTaggedReleaseVersion(release.TagName) {
		return githubRelease{}, fmt.Errorf("latest release tag %q is not a supported version", release.TagName)
	}

	return release, nil
}

func selectReleaseAssets(release githubRelease, goos, goarch string) (releaseAsset, releaseAsset, error) {
	expectedAssetName, err := releaseAssetName(release.TagName, goos, goarch)
	if err != nil {
		return releaseAsset{}, releaseAsset{}, err
	}

	var binaryAsset releaseAsset
	var checksumAsset releaseAsset
	for _, asset := range release.Assets {
		switch asset.Name {
		case expectedAssetName:
			binaryAsset = asset
		case checksumsAssetName:
			checksumAsset = asset
		}
	}

	if binaryAsset.Name == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("release %s does not contain a %s asset", release.TagName, expectedAssetName)
	}
	if checksumAsset.Name == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("release %s does not contain %s", release.TagName, checksumsAssetName)
	}

	return binaryAsset, checksumAsset, nil
}

func releaseAssetName(version, goos, goarch string) (string, error) {
	switch {
	case goos == "linux" && goarch == "amd64":
		return fmt.Sprintf("deploy_%s_linux_amd64", version), nil
	case goos == "darwin" && goarch == "amd64":
		return fmt.Sprintf("deploy_%s_darwin_amd64", version), nil
	case goos == "darwin" && goarch == "arm64":
		return fmt.Sprintf("deploy_%s_darwin_arm64", version), nil
	case goos == "windows" && goarch == "amd64":
		return fmt.Sprintf("deploy_%s_windows_amd64.exe", version), nil
	default:
		return "", fmt.Errorf("no release asset is defined for %s/%s", goos, goarch)
	}
}

func (u *updater) downloadText(url string) (string, error) {
	body, err := u.getURLBytes(url, "release checksums", "")
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func (u *updater) downloadAssetToTemp(asset releaseAsset) (string, error) {
	body, err := u.getURLBytes(asset.BrowserDownloadURL, asset.Name, "")
	if err != nil {
		return "", err
	}

	file, err := os.CreateTemp("", "deploy-update-*")
	if err != nil {
		return "", err
	}
	defer file.Close()

	if _, err := file.Write(body); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}

	return file.Name(), nil
}

func verifyChecksumFile(path, assetName, checksums string) error {
	expectedChecksum, err := parseChecksum(checksums, assetName)
	if err != nil {
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}

	actualChecksum := hex.EncodeToString(hash.Sum(nil))
	if actualChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch for %s", assetName)
	}

	return nil
}

func parseChecksum(checksums, assetName string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if fields[1] == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}

	return "", fmt.Errorf("checksum for %s was not found", assetName)
}

func (u *updater) cachePath() (string, error) {
	root, err := u.cacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "deploy", "update-check.json"), nil
}

func (u *updater) loadCache() (updateCacheState, bool, error) {
	path, err := u.cachePath()
	if err != nil {
		return updateCacheState{}, false, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return updateCacheState{}, false, nil
		}
		return updateCacheState{}, false, err
	}

	var state updateCacheState
	if err := json.Unmarshal(data, &state); err != nil {
		return updateCacheState{}, false, err
	}

	return state, true, nil
}

func (u *updater) saveCache(state updateCacheState) error {
	path, err := u.cachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o644)
}

func (u *updater) getURLBytes(url, description, accept string) ([]byte, error) {
	var lastErr error
	var timeout bool

	for attempt := 1; attempt <= updateMaxAttempts; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		req.Header.Set("User-Agent", "deploy/"+version)

		resp, err := u.client.Do(req)
		if err != nil {
			lastErr = err
			timeout = timeout || isTimeoutError(err)
			if !isRetryableNetworkError(err) || attempt == updateMaxAttempts {
				break
			}
			updateSleep(updateRetryDelay)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt == updateMaxAttempts {
				break
			}
			updateSleep(updateRetryDelay)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s", resp.Status)
			if !isRetryableHTTPStatus(resp.StatusCode) || attempt == updateMaxAttempts {
				break
			}
			updateSleep(updateRetryDelay)
			continue
		}

		return body, nil
	}

	if lastErr == nil {
		lastErr = errors.New("request failed")
	}
	if timeout {
		return nil, fmt.Errorf("timed out contacting GitHub for %s after %d attempts: %w", description, updateMaxAttempts, lastErr)
	}
	if isRetryableNetworkError(lastErr) {
		return nil, fmt.Errorf("failed to download %s after %d attempts: %w", description, updateMaxAttempts, lastErr)
	}
	return nil, fmt.Errorf("failed to download %s: %w", description, lastErr)
}

func isTaggedReleaseVersion(value string) bool {
	_, err := parseSemver(value)
	return err == nil
}

func isNewerVersion(candidate, current string) (bool, error) {
	candidateSemver, err := parseSemver(candidate)
	if err != nil {
		return false, err
	}
	currentSemver, err := parseSemver(current)
	if err != nil {
		return false, err
	}

	return compareSemver(candidateSemver, currentSemver) > 0, nil
}

func parseSemver(value string) (semver, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid semantic version %q", value)
	}

	parsed := semver{}
	values := []*int{&parsed.major, &parsed.minor, &parsed.patch}
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return semver{}, fmt.Errorf("invalid semantic version %q", value)
		}
		*values[index] = number
	}

	return parsed, nil
}

func compareSemver(left, right semver) int {
	switch {
	case left.major != right.major:
		return compareInts(left.major, right.major)
	case left.minor != right.minor:
		return compareInts(left.minor, right.minor)
	default:
		return compareInts(left.patch, right.patch)
	}
}

func compareInts(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func isRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if isTimeoutError(err) {
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr)
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isRetryableHTTPStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusInternalServerError:
		return true
	default:
		return false
	}
}

func spawnWindowsReplaceHelper(currentExecutable, sourcePath, targetPath string) error {
	helperDir, err := os.MkdirTemp("", "deploy-update-helper-*")
	if err != nil {
		return err
	}

	helperPath := filepath.Join(helperDir, "deploy-helper.exe")
	if _, err := installExecutable(currentExecutable, helperPath, "windows"); err != nil {
		return err
	}

	cmd := exec.Command(helperPath, internalWindowsReplaceCommand, sourcePath, targetPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}

	return nil
}

func runWindowsReplaceHelper(args []string) error {
	if runtime.GOOS != "windows" {
		return errors.New("internal replace helper is only supported on windows")
	}
	if len(args) != 2 {
		return errors.New("internal replace helper requires source and target paths")
	}

	sourcePath := args[0]
	targetPath := args[1]
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error

	for time.Now().Before(deadline) {
		if _, err := installExecutable(sourcePath, targetPath, "windows"); err == nil {
			_ = os.Remove(sourcePath)
			return nil
		} else {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
		}
	}

	if lastErr == nil {
		lastErr = errors.New("timed out waiting to replace the executable")
	}
	return lastErr
}

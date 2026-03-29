package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestParseDotEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "PROJECT=my-app\nSSH_URI=user@example.com\nPASSWORD=\"abc#123\"\nEMPTY=\nPLAIN=hello # comment\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	values, err := parseDotEnvFile(path)
	if err != nil {
		t.Fatalf("parseDotEnvFile returned error: %v", err)
	}

	want := map[string]string{
		"PROJECT":  "my-app",
		"SSH_URI":  "user@example.com",
		"PASSWORD": "abc#123",
		"EMPTY":    "",
		"PLAIN":    "hello",
	}

	if !reflect.DeepEqual(values, want) {
		t.Fatalf("unexpected values: got %#v want %#v", values, want)
	}
}

func TestParseDotEnvFileRejectsShellSyntax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("PROJECT=$(whoami)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := parseDotEnvFile(path); err == nil {
		t.Fatal("expected parseDotEnvFile to reject shell syntax")
	}
}

func TestNormalizeSSHURI(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantDocker string
		wantTarget string
	}{
		{
			name:       "bare host",
			input:      "user@example.com",
			wantDocker: "ssh://user@example.com:22",
			wantTarget: "user@example.com:22",
		},
		{
			name:       "prefixed host",
			input:      "ssh://user@example.com:2200",
			wantDocker: "ssh://user@example.com:2200",
			wantTarget: "user@example.com:2200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDocker, gotTarget, err := normalizeSSHURI(tt.input)
			if err != nil {
				t.Fatalf("normalizeSSHURI returned error: %v", err)
			}
			if gotDocker != tt.wantDocker || gotTarget != tt.wantTarget {
				t.Fatalf("unexpected normalization: got (%q, %q) want (%q, %q)", gotDocker, gotTarget, tt.wantDocker, tt.wantTarget)
			}
		})
	}
}

func TestDiscoverFilesPrefersDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("PROJECT=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "prod.env"), []byte("PROJECT=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chdir(previous)
	}()

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	got, err := discoverFiles([]string{"./.env"}, []string{"*.env", ".env.*"})
	if err != nil {
		t.Fatalf("discoverFiles returned error: %v", err)
	}

	want := []string{"./.env", "./nested/prod.env"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected file order: got %#v want %#v", got, want)
	}
}

func TestComposeFileHasTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yml")
	content := "version: \"3.9\"\nname: test\nservices: {}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	hasVersion, err := composeFileHasTopLevelKey(path, "version")
	if err != nil {
		t.Fatalf("composeFileHasTopLevelKey returned error: %v", err)
	}
	if !hasVersion {
		t.Fatal("expected version key to be detected")
	}
}

func TestMenuModelNavigationAndSelection(t *testing.T) {
	model := newMenuModel("Title", "Prompt", []menuOption{
		{label: "first"},
		{label: "second"},
	})

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(menuModel)
	if model.cursor != 1 {
		t.Fatalf("expected cursor to move down, got %d", model.cursor)
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(menuModel)
	if model.choice != "second" {
		t.Fatalf("expected selected choice to be second, got %q", model.choice)
	}
	if model.cancelled {
		t.Fatal("expected selection, got cancelled state")
	}
}

func TestMenuModelCancel(t *testing.T) {
	model := newMenuModel("Title", "Prompt", []menuOption{
		{label: "first"},
	})

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(menuModel)
	if !model.cancelled {
		t.Fatal("expected menu to cancel on esc")
	}
}

func TestBuildSystemSSHArgs(t *testing.T) {
	tests := []struct {
		name string
		spec sshSpec
		want []string
	}{
		{
			name: "default port",
			spec: sshSpec{user: "alice", host: "example.com", port: "22"},
			want: []string{"alice@example.com"},
		},
		{
			name: "custom port",
			spec: sshSpec{user: "alice", host: "example.com", port: "2200"},
			want: []string{"-p", "2200", "alice@example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSystemSSHArgs(tt.spec)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unexpected ssh args: got %#v want %#v", got, tt.want)
			}
		})
	}
}

func TestParseCLIArgsDefaults(t *testing.T) {
	options, err := parseCLIArgs(nil)
	if err != nil {
		t.Fatalf("parseCLIArgs returned error: %v", err)
	}
	if options.profile != "dev" {
		t.Fatalf("expected default profile dev, got %q", options.profile)
	}
	if options.showHelp {
		t.Fatal("did not expect help flag to be set")
	}
}

func TestParseCLIArgsSupportsFlagsAndPositionalProfile(t *testing.T) {
	t.Run("profile flags", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"-p", "prod", "-e", ".env.prod", "-f", "compose.yml"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if options.profile != "prod" || options.envFile != ".env.prod" || options.composeFile != "compose.yml" {
			t.Fatalf("unexpected options: %#v", options)
		}
	})

	t.Run("positional profile", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"prod"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if options.profile != "prod" {
			t.Fatalf("expected positional profile prod, got %q", options.profile)
		}
	})
}

func TestParseCLIArgsHelpAndConflicts(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--help"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.showHelp {
			t.Fatal("expected help flag to be set")
		}
	})

	t.Run("version", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--version"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.showVersion {
			t.Fatal("expected version flag to be set")
		}
	})

	t.Run("conflicting profile values", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--profile", "prod", "dev"}); err == nil {
			t.Fatal("expected conflicting profile values to return an error")
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--nope"}); err == nil {
			t.Fatal("expected unknown flag to return an error")
		}
	})

	t.Run("install long flag", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--install"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.install {
			t.Fatal("expected install flag to be set")
		}
	})

	t.Run("install short flag", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"-i"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.install {
			t.Fatal("expected install flag to be set")
		}
	})

	t.Run("install conflicts with profile flag", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--install", "--profile", "prod"}); err == nil {
			t.Fatal("expected install/profile conflict to return an error")
		}
	})

	t.Run("install conflicts with env flag", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--install", "--env", ".env.prod"}); err == nil {
			t.Fatal("expected install/env conflict to return an error")
		}
	})

	t.Run("install conflicts with positional profile", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--install", "prod"}); err == nil {
			t.Fatal("expected install/positional profile conflict to return an error")
		}
	})
}

func TestValidateInputFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.env")
	if err := os.WriteFile(filePath, []byte("PROJECT=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := validateInputFile(filePath, "ENV")
	if err != nil {
		t.Fatalf("validateInputFile returned error: %v", err)
	}
	if got != filePath {
		t.Fatalf("expected validated path %q, got %q", filePath, got)
	}

	if _, err := validateInputFile(filepath.Join(dir, "missing.env"), "ENV"); err == nil {
		t.Fatal("expected missing file to return an error")
	}
}

func TestPrintUsage(t *testing.T) {
	var out bytes.Buffer
	application := &app{stdout: &out}
	application.printUsage(&out)

	text := out.String()
	if !strings.Contains(text, "deploy [flags]") {
		t.Fatalf("expected usage text to mention flags, got %q", text)
	}
	if !strings.Contains(text, "--profile") || !strings.Contains(text, "--help") || !strings.Contains(text, "--install") || !strings.Contains(text, "--version") {
		t.Fatalf("expected usage text to mention help, profile, install, and version flags, got %q", text)
	}
}

func TestRunVersion(t *testing.T) {
	previousVersion := version
	version = "v1.2.3"
	defer func() {
		version = previousVersion
	}()

	var stdout bytes.Buffer
	application := &app{
		reader: bufio.NewReader(strings.NewReader("")),
		stdout: &stdout,
		stderr: io.Discard,
	}

	if err := application.run([]string{"--version"}); err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if got := stdout.String(); got != "deploy v1.2.3\n" {
		t.Fatalf("unexpected version output %q", got)
	}
}

func TestResolveInstallTarget(t *testing.T) {
	t.Run("prefers writable user path entry", func(t *testing.T) {
		env := map[string]string{"HOME": "/home/alice"}
		pathValue := "/usr/local/bin:/home/alice/bin:/home/alice/.local/bin"
		target, err := resolveInstallTarget("linux", env, pathValue, func(path string) bool {
			return normalizePathForOS("linux", path) == normalizePathForOS("linux", "/home/alice/.local/bin")
		})
		if err != nil {
			t.Fatalf("resolveInstallTarget returned error: %v", err)
		}
		if target.dir != "/home/alice/.local/bin" {
			t.Fatalf("expected /home/alice/.local/bin, got %q", target.dir)
		}
		if target.path != "/home/alice/.local/bin/deploy" {
			t.Fatalf("unexpected target path %q", target.path)
		}
		if target.pathInstruction != "" {
			t.Fatalf("did not expect PATH instruction, got %q", target.pathInstruction)
		}
	})

	t.Run("skips shared path entries and falls back on linux", func(t *testing.T) {
		env := map[string]string{"HOME": "/home/alice"}
		target, err := resolveInstallTarget("linux", env, "/usr/local/bin:/opt/bin", func(string) bool {
			return true
		})
		if err != nil {
			t.Fatalf("resolveInstallTarget returned error: %v", err)
		}
		if target.dir != "/home/alice/.local/bin" {
			t.Fatalf("expected linux fallback dir, got %q", target.dir)
		}
		if target.pathInstruction != `export PATH="$HOME/.local/bin:$PATH"` {
			t.Fatalf("unexpected PATH instruction %q", target.pathInstruction)
		}
	})

	t.Run("uses xdg bin home when set", func(t *testing.T) {
		env := map[string]string{
			"HOME":         "/home/alice",
			"XDG_BIN_HOME": "/home/alice/.bin",
		}
		target, err := resolveInstallTarget("linux", env, "/usr/local/bin", func(string) bool {
			return false
		})
		if err != nil {
			t.Fatalf("resolveInstallTarget returned error: %v", err)
		}
		if target.dir != "/home/alice/.bin" {
			t.Fatalf("expected XDG fallback dir, got %q", target.dir)
		}
		if target.pathInstruction != `export PATH="/home/alice/.bin:$PATH"` {
			t.Fatalf("unexpected PATH instruction %q", target.pathInstruction)
		}
	})

	t.Run("uses macos fallback", func(t *testing.T) {
		env := map[string]string{"HOME": "/Users/alice"}
		target, err := resolveInstallTarget("darwin", env, "/usr/local/bin", func(string) bool {
			return false
		})
		if err != nil {
			t.Fatalf("resolveInstallTarget returned error: %v", err)
		}
		if target.dir != "/Users/alice/.local/bin" {
			t.Fatalf("expected darwin fallback dir, got %q", target.dir)
		}
		if target.path != "/Users/alice/.local/bin/deploy" {
			t.Fatalf("unexpected target path %q", target.path)
		}
	})

	t.Run("uses windows fallback", func(t *testing.T) {
		env := map[string]string{
			"USERPROFILE":  `C:\Users\Alice`,
			"LOCALAPPDATA": `C:\Users\Alice\AppData\Local`,
		}
		target, err := resolveInstallTarget("windows", env, `C:\Windows\System32`, func(string) bool {
			return false
		})
		if err != nil {
			t.Fatalf("resolveInstallTarget returned error: %v", err)
		}
		if target.dir != `C:\Users\Alice\bin` {
			t.Fatalf("expected windows fallback dir, got %q", target.dir)
		}
		if target.path != `C:\Users\Alice\bin\deploy.exe` {
			t.Fatalf("unexpected target path %q", target.path)
		}
		if target.pathInstruction != `set PATH=%USERPROFILE%\bin;%PATH%` {
			t.Fatalf("unexpected PATH instruction %q", target.pathInstruction)
		}
	})
}

func TestInstallExecutable(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "deploy-source")
	targetPath := filepath.Join(dir, executableName(runtime.GOOS))

	if err := os.WriteFile(sourcePath, []byte("first"), 0o700); err != nil {
		t.Fatal(err)
	}

	installed, err := installExecutable(sourcePath, targetPath, runtime.GOOS)
	if err != nil {
		t.Fatalf("installExecutable returned error: %v", err)
	}
	if !installed {
		t.Fatal("expected first install to copy the executable")
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" {
		t.Fatalf("unexpected installed contents %q", string(data))
	}

	if err := os.WriteFile(sourcePath, []byte("second"), 0o700); err != nil {
		t.Fatal(err)
	}

	installed, err = installExecutable(sourcePath, targetPath, runtime.GOOS)
	if err != nil {
		t.Fatalf("installExecutable overwrite returned error: %v", err)
	}
	if !installed {
		t.Fatal("expected overwrite install to copy the executable")
	}

	data, err = os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("unexpected overwritten contents %q", string(data))
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("expected installed mode 0755, got %o", info.Mode().Perm())
		}
	}
}

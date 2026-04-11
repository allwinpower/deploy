package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestBuildSystemSSHTunnelArgs(t *testing.T) {
	tests := []struct {
		name string
		spec sshSpec
		want []string
	}{
		{
			name: "default port",
			spec: sshSpec{user: "alice", host: "example.com", port: "22"},
			want: []string{"-N", "-S", "none", "-o", "ControlMaster=no", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1", "-L", "15432:127.0.0.1:15432", "alice@example.com"},
		},
		{
			name: "custom port",
			spec: sshSpec{user: "alice", host: "example.com", port: "2200"},
			want: []string{"-p", "2200", "-N", "-S", "none", "-o", "ControlMaster=no", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1", "-L", "15432:127.0.0.1:15432", "alice@example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSystemSSHTunnelArgs(tt.spec, databaseManagementPort, databaseManagementPort)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unexpected ssh tunnel args: got %#v want %#v", got, tt.want)
			}
		})
	}
}

func TestBuildSystemSCPArgs(t *testing.T) {
	got := buildSystemSCPArgs(
		sshSpec{user: "alice", host: "example.com", port: "2200"},
		"/tmp/local-secret",
		"alice@example.com:/home/alice/.deploy_secrets/app/gcp_sa",
	)
	want := []string{"-P", "2200", "/tmp/local-secret", "alice@example.com:/home/alice/.deploy_secrets/app/gcp_sa"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected scp args: got %#v want %#v", got, want)
	}
}

func TestRemoteSecretPathSanitizesSegments(t *testing.T) {
	got := remoteSecretPath("alice@example.com", "my/app", "gcp.sa")
	want := "/home/alice/.deploy_secrets/my_app/gcp.sa"
	if got != want {
		t.Fatalf("unexpected remote secret path: got %q want %q", got, want)
	}
}

func TestWriteSecretOverrideFile(t *testing.T) {
	path, err := writeSecretOverrideFile(map[string]string{
		"gcp_sa": "/home/alice/.deploy_secrets/mediapilot/gcp_sa",
	})
	if err != nil {
		t.Fatalf("writeSecretOverrideFile returned error: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read override file: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "gcp_sa:") || !strings.Contains(text, "/home/alice/.deploy_secrets/mediapilot/gcp_sa") {
		t.Fatalf("override file did not contain expected secret mapping:\n%s", text)
	}
}

func TestEnsureLocalPortAvailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := ensureLocalPortAvailable(port); err == nil {
		t.Fatal("expected occupied port to be unavailable")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalPortAvailable(port); err != nil {
		t.Fatalf("expected released port to be available: %v", err)
	}
}

func TestBrowserCommand(t *testing.T) {
	tests := []struct {
		name        string
		goos        string
		wantCommand string
		wantArgs    []string
	}{
		{
			name:        "linux",
			goos:        "linux",
			wantCommand: "xdg-open",
			wantArgs:    []string{"http://127.0.0.1:15432"},
		},
		{
			name:        "macos",
			goos:        "darwin",
			wantCommand: "open",
			wantArgs:    []string{"http://127.0.0.1:15432"},
		},
		{
			name:        "windows",
			goos:        "windows",
			wantCommand: "rundll32",
			wantArgs:    []string{"url.dll,FileProtocolHandler", "http://127.0.0.1:15432"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCommand, gotArgs, err := browserCommand("http://127.0.0.1:15432", tt.goos)
			if err != nil {
				t.Fatalf("browserCommand returned error: %v", err)
			}
			if gotCommand != tt.wantCommand || !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("unexpected browser command: got (%q, %#v) want (%q, %#v)", gotCommand, gotArgs, tt.wantCommand, tt.wantArgs)
			}
		})
	}
}

func TestWaitForDatabaseManagementHealthRequiresMarker(t *testing.T) {
	t.Run("accepts marker", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("X-Db-Admin-Session"); got != "token" {
				t.Fatalf("missing session header: %q", got)
			}
			_, _ = w.Write([]byte(`{"ok":true,"service":"postgres-db-admin"}`))
		}))
		defer server.Close()

		if err := waitForDatabaseManagementHealth(server.URL, "token", time.Second); err != nil {
			t.Fatalf("expected health success: %v", err)
		}
	})

	t.Run("rejects wrong service", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true,"service":"other"}`))
		}))
		defer server.Close()

		if err := waitForDatabaseManagementHealth(server.URL, "token", 20*time.Millisecond); err == nil {
			t.Fatal("expected wrong service marker to fail")
		}
	})
}

func TestComposeBuildArgs(t *testing.T) {
	t.Run("all services default cache", func(t *testing.T) {
		got := composeBuildArgs("", false)
		want := []string{"build"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected build args: got %#v want %#v", got, want)
		}
	})

	t.Run("single service default cache", func(t *testing.T) {
		got := composeBuildArgs("web", false)
		want := []string{"build", "web"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected build args: got %#v want %#v", got, want)
		}
	})

	t.Run("all services no cache", func(t *testing.T) {
		got := composeBuildArgs("", true)
		want := []string{"build", "--no-cache"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected build args: got %#v want %#v", got, want)
		}
	})

	t.Run("single service no cache", func(t *testing.T) {
		got := composeBuildArgs("web", true)
		want := []string{"build", "--no-cache", "web"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected build args: got %#v want %#v", got, want)
		}
	})
}

func TestComposePrepareArgs(t *testing.T) {
	buildModel := composeModel{Services: map[string]composeService{
		"api": {HasBuild: true},
	}}
	imageModel := composeModel{Services: map[string]composeService{
		"api": {Image: "ghcr.io/example/api:prod"},
	}}
	mixedModel := composeModel{Services: map[string]composeService{
		"api": {Image: "ghcr.io/example/api:prod"},
		"web": {HasBuild: true},
	}}

	tests := []struct {
		name    string
		model   composeModel
		service string
		noCache bool
		want    []string
	}{
		{
			name:  "all build services use build",
			model: buildModel,
			want:  []string{"build"},
		},
		{
			name:    "all build services respect no cache",
			model:   buildModel,
			noCache: true,
			want:    []string{"build", "--no-cache"},
		},
		{
			name:  "all image services use pull",
			model: imageModel,
			want:  []string{"pull"},
		},
		{
			name:    "single image service uses pull",
			model:   imageModel,
			service: "api",
			want:    []string{"pull", "api"},
		},
		{
			name:    "single build service uses build",
			model:   mixedModel,
			service: "web",
			want:    []string{"build", "web"},
		},
		{
			name:  "mixed all services use build",
			model: mixedModel,
			want:  []string{"build"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := composePrepareArgs(tt.model, tt.service, tt.noCache)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unexpected prepare args: got %#v want %#v", got, tt.want)
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
	if options.forceLocal {
		t.Fatal("did not expect forceLocal to be set by default")
	}
	if options.forceRemote {
		t.Fatal("did not expect forceRemote to be set by default")
	}
	if options.deployAll {
		t.Fatal("did not expect deployAll to be set by default")
	}
	if options.undeployAll {
		t.Fatal("did not expect undeployAll to be set by default")
	}
	if options.deleteVolumes {
		t.Fatal("did not expect deleteVolumes to be set by default")
	}
	if options.noCache {
		t.Fatal("did not expect noCache to be set by default")
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

	t.Run("local long flag", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--local", "-p", "dev"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.forceLocal || options.profile != "dev" {
			t.Fatalf("unexpected options: %#v", options)
		}
	})

	t.Run("local short flag", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"-l", "-e", ".env", "-f", "compose.yml"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.forceLocal || options.envFile != ".env" || options.composeFile != "compose.yml" {
			t.Fatalf("unexpected options: %#v", options)
		}
	})

	t.Run("no cache flag", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--no-cache", "-p", "prod"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.noCache || options.profile != "prod" {
			t.Fatalf("unexpected options: %#v", options)
		}
	})

	t.Run("remote flag", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--remote", "-p", "prod"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.forceRemote || options.profile != "prod" {
			t.Fatalf("unexpected options: %#v", options)
		}
	})

	t.Run("deploy all flag", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--deploy-all", "--local", "prod"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.deployAll || !options.forceLocal || options.profile != "prod" {
			t.Fatalf("unexpected options: %#v", options)
		}
	})

	t.Run("undeploy all with delete volumes", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--undeploy-all", "--delete-volumes"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.undeployAll || !options.deleteVolumes {
			t.Fatalf("unexpected options: %#v", options)
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

	t.Run("update", func(t *testing.T) {
		options, err := parseCLIArgs([]string{"--update"})
		if err != nil {
			t.Fatalf("parseCLIArgs returned error: %v", err)
		}
		if !options.update {
			t.Fatal("expected update flag to be set")
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

	t.Run("install conflicts with local flag", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--install", "--local"}); err == nil {
			t.Fatal("expected install/local conflict to return an error")
		}
	})

	t.Run("update conflicts with local flag", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--update", "--local"}); err == nil {
			t.Fatal("expected update/local conflict to return an error")
		}
	})

	t.Run("update conflicts with profile flag", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--update", "--profile", "prod"}); err == nil {
			t.Fatal("expected update/profile conflict to return an error")
		}
	})

	t.Run("update conflicts with positional profile", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--update", "prod"}); err == nil {
			t.Fatal("expected update/positional profile conflict to return an error")
		}
	})

	t.Run("update conflicts with version", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--update", "--version"}); err == nil {
			t.Fatal("expected update/version conflict to return an error")
		}
	})

	t.Run("local conflicts with remote", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--local", "--remote"}); err == nil {
			t.Fatal("expected local/remote conflict to return an error")
		}
	})

	t.Run("deploy all conflicts with undeploy all", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--deploy-all", "--undeploy-all"}); err == nil {
			t.Fatal("expected deploy-all/undeploy-all conflict to return an error")
		}
	})

	t.Run("delete volumes requires undeploy all", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--delete-volumes"}); err == nil {
			t.Fatal("expected delete-volumes without undeploy-all to return an error")
		}
	})

	t.Run("direct action conflicts with install", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--deploy-all", "--install"}); err == nil {
			t.Fatal("expected direct action/install conflict to return an error")
		}
	})

	t.Run("direct action conflicts with help", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--deploy-all", "--help"}); err == nil {
			t.Fatal("expected direct action/help conflict to return an error")
		}
	})

	t.Run("direct action conflicts with version", func(t *testing.T) {
		if _, err := parseCLIArgs([]string{"--undeploy-all", "--version"}); err == nil {
			t.Fatal("expected direct action/version conflict to return an error")
		}
	})
}

func TestCLIOptionsDirectAction(t *testing.T) {
	t.Run("deploy all", func(t *testing.T) {
		got, ok := (cliOptions{deployAll: true}).directAction()
		if !ok || got != "Deploy All Services" {
			t.Fatalf("unexpected direct action: got (%q, %t)", got, ok)
		}
	})

	t.Run("undeploy all keeps volumes by default", func(t *testing.T) {
		got, ok := (cliOptions{undeployAll: true}).directAction()
		if !ok || got != "Undeploy All Services (Keep Volumes)" {
			t.Fatalf("unexpected direct action: got (%q, %t)", got, ok)
		}
	})

	t.Run("undeploy all delete volumes", func(t *testing.T) {
		got, ok := (cliOptions{undeployAll: true, deleteVolumes: true}).directAction()
		if !ok || got != "Undeploy All Services" {
			t.Fatalf("unexpected direct action: got (%q, %t)", got, ok)
		}
	})
}

func TestResolveDeploymentTargetHelpers(t *testing.T) {
	t.Run("local target without ssh uri", func(t *testing.T) {
		got, err := resolveLocalDeploymentTarget("")
		if err != nil {
			t.Fatalf("resolveLocalDeploymentTarget returned error: %v", err)
		}
		if got.label != "Locally" || got.dockerHost != "" || got.sshTarget != "" {
			t.Fatalf("unexpected target: %#v", got)
		}
	})

	t.Run("local target preserves ssh host shell target", func(t *testing.T) {
		got, err := resolveLocalDeploymentTarget("user@example.com")
		if err != nil {
			t.Fatalf("resolveLocalDeploymentTarget returned error: %v", err)
		}
		if got.label != "Locally" || got.dockerHost != "" || got.sshTarget != "user@example.com:22" {
			t.Fatalf("unexpected target: %#v", got)
		}
	})

	t.Run("remote target requires ssh uri", func(t *testing.T) {
		if _, err := resolveRemoteDeploymentTarget(""); err == nil {
			t.Fatal("expected resolveRemoteDeploymentTarget to require SSH_URI")
		}
	})

	t.Run("remote target normalizes ssh uri", func(t *testing.T) {
		got, err := resolveRemoteDeploymentTarget("user@example.com")
		if err != nil {
			t.Fatalf("resolveRemoteDeploymentTarget returned error: %v", err)
		}
		if got.label != "user@example.com" || got.dockerHost != "ssh://user@example.com:22" || got.sshTarget != "user@example.com:22" {
			t.Fatalf("unexpected target: %#v", got)
		}
	})
}

func TestResolveDeploymentTarget(t *testing.T) {
	t.Run("direct action without ssh defaults locally", func(t *testing.T) {
		var stdout bytes.Buffer
		application := &app{stdout: &stdout}
		got, err := application.resolveDeploymentTarget("", cliOptions{deployAll: true})
		if err != nil {
			t.Fatalf("resolveDeploymentTarget returned error: %v", err)
		}
		if got.label != "Locally" {
			t.Fatalf("unexpected target: %#v", got)
		}
		if !strings.Contains(stdout.String(), "Selected deployment method: Locally") {
			t.Fatalf("expected local selection output, got %q", stdout.String())
		}
	})

	t.Run("direct action with ssh requires explicit target", func(t *testing.T) {
		application := &app{stdout: io.Discard}
		if _, err := application.resolveDeploymentTarget("user@example.com", cliOptions{deployAll: true}); err == nil {
			t.Fatal("expected direct action with SSH_URI to require an explicit target")
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
	if !strings.Contains(text, "--profile") || !strings.Contains(text, "--help") || !strings.Contains(text, "--install") || !strings.Contains(text, "--version") || !strings.Contains(text, "--update") || !strings.Contains(text, "--no-cache") || !strings.Contains(text, "--remote") || !strings.Contains(text, "--deploy-all") || !strings.Contains(text, "--undeploy-all") || !strings.Contains(text, "--delete-volumes") {
		t.Fatalf("expected usage text to mention help, profile, install, version, and update flags, got %q", text)
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

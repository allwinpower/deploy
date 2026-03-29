package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

type app struct {
	reader *bufio.Reader
	stdin  *os.File
	stdout io.Writer
	stderr io.Writer
}

type composeContext struct {
	envFile     string
	composeFile string
	envValues   map[string]string
	project     string
	sshURI      string
	model       composeModel
	target      deploymentTarget
}

type deploymentTarget struct {
	label      string
	dockerHost string
	sshTarget  string
}

type composeModel struct {
	Name     string                     `json:"name"`
	Services map[string]json.RawMessage `json:"services"`
	Networks map[string]composeNetwork  `json:"networks"`
}

type composeNetwork struct {
	Name     string `json:"name"`
	External bool   `json:"external"`
}

type sshSpec struct {
	user string
	host string
	port string
}

var errSelectionCancelled = errors.New("selection cancelled")
var version = "dev"

type cliOptions struct {
	profile     string
	envFile     string
	composeFile string
	install     bool
	update      bool
	showHelp    bool
	showVersion bool
}

type stringFlag struct {
	value string
	set   bool
}

func (f *stringFlag) String() string {
	return f.value
}

func (f *stringFlag) Set(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return errors.New("flag value cannot be empty")
	}
	f.value = trimmed
	f.set = true
	return nil
}

func main() {
	application := &app{
		reader: bufio.NewReader(os.Stdin),
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
	}

	if err := application.run(os.Args[1:]); err != nil {
		fmt.Fprintln(application.stderr, err)
		os.Exit(1)
	}
}

func (a *app) run(args []string) error {
	if len(args) > 0 && args[0] == internalWindowsReplaceCommand {
		return runWindowsReplaceHelper(args[1:])
	}

	options, err := parseCLIArgs(args)
	if err != nil {
		a.printUsage(a.stderr)
		return err
	}
	if options.showHelp {
		a.printUsage(a.stdout)
		return nil
	}
	if options.showVersion {
		fmt.Fprintln(a.stdout, versionString())
		return nil
	}
	if options.install {
		return a.installSelf()
	}
	if options.update {
		return a.runSelfUpdate()
	}
	if handled, err := a.checkForStartupUpdate(); err != nil {
		return err
	} else if handled {
		return nil
	}

	fmt.Fprintln(a.stdout, "Starting Docker Compose Management Script...")

	envFile, err := a.resolveInputFile(
		options.envFile,
		"ENV",
		[]string{"./.env"},
		[]string{"*.env", ".env.*"},
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Using ENV file: %s\n", envFile)

	envValues, err := parseDotEnvFile(envFile)
	if err != nil {
		return fmt.Errorf("failed to parse env file %s: %w", envFile, err)
	}

	project := strings.TrimSpace(envValues["PROJECT"])
	if project == "" {
		a.msgBox("Missing PROJECT", fmt.Sprintf("The selected env file does not set PROJECT. Set PROJECT in %s before using this script.", envFile))
		return errors.New("missing PROJECT in env file")
	}

	composeFile, err := a.resolveInputFile(
		options.composeFile,
		"Compose",
		[]string{
			"./docker-compose.yml",
			"./docker-compose.yaml",
			"./compose.yml",
			"./compose.yaml",
		},
		[]string{"*.yml", "*.yaml"},
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Using compose file: %s\n", composeFile)

	composeEnv := buildComposeEnv(envValues, options.profile, "")

	hasVersion, err := composeFileHasTopLevelKey(composeFile, "version")
	if err != nil {
		return fmt.Errorf("failed to inspect compose file metadata: %w", err)
	}
	if hasVersion {
		a.msgBox("Obsolete Compose Version", "The selected compose file sets a top-level version. This is not part of the current Compose standard. Remove it.")
	}

	if err := validateCompose(composeEnv, envFile, composeFile); err != nil {
		return err
	}

	model, err := loadComposeModel(composeEnv, envFile, composeFile)
	if err != nil {
		return err
	}

	if strings.TrimSpace(model.Name) == "" {
		a.msgBox("Missing Compose Name", fmt.Sprintf("The selected compose file must set a top-level name that matches PROJECT (%s).", project))
		return errors.New("compose file missing top-level name")
	}
	if model.Name != project {
		a.msgBox("Compose Name Mismatch", fmt.Sprintf("Compose name %q does not match PROJECT %q. Update the compose file or env file before using this script.", model.Name, project))
		return errors.New("compose name does not match PROJECT")
	}

	ctx := composeContext{
		envFile:     envFile,
		composeFile: composeFile,
		envValues:   envValues,
		project:     project,
		sshURI:      strings.TrimSpace(envValues["SSH_URI"]),
		model:       model,
	}

	target, err := a.selectDeploymentTarget(ctx.sshURI)
	if err != nil {
		return err
	}
	ctx.target = target

	action, err := a.selectMainAction(ctx)
	if err != nil {
		return err
	}

	if err := a.executeAction(ctx, options.profile, action); err != nil {
		return err
	}

	fmt.Fprintln(a.stdout, "Script finished.")
	return nil
}

func parseCLIArgs(args []string) (cliOptions, error) {
	options := cliOptions{profile: "dev"}

	var profileFlag stringFlag
	var envFlag stringFlag
	var fileFlag stringFlag
	var install bool
	var update bool
	var help bool
	var showVersion bool

	flagSet := flag.NewFlagSet("deploy", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	flagSet.Var(&profileFlag, "p", "")
	flagSet.Var(&profileFlag, "profile", "")
	flagSet.Var(&envFlag, "e", "")
	flagSet.Var(&envFlag, "env", "")
	flagSet.Var(&fileFlag, "f", "")
	flagSet.Var(&fileFlag, "file", "")
	flagSet.BoolVar(&install, "i", false, "")
	flagSet.BoolVar(&install, "install", false, "")
	flagSet.BoolVar(&update, "u", false, "")
	flagSet.BoolVar(&update, "update", false, "")
	flagSet.BoolVar(&help, "h", false, "")
	flagSet.BoolVar(&help, "help", false, "")
	flagSet.BoolVar(&showVersion, "v", false, "")
	flagSet.BoolVar(&showVersion, "version", false, "")

	if err := flagSet.Parse(args); err != nil {
		return options, err
	}

	if help {
		options.showHelp = true
	}
	if showVersion {
		options.showVersion = true
	}

	if profileFlag.set {
		options.profile = profileFlag.value
	}
	if envFlag.set {
		options.envFile = envFlag.value
	}
	if fileFlag.set {
		options.composeFile = fileFlag.value
	}
	if install {
		options.install = true
	}
	if update {
		options.update = true
	}

	remaining := flagSet.Args()
	if options.install {
		if profileFlag.set {
			return options, errors.New("--install cannot be combined with --profile")
		}
		if envFlag.set {
			return options, errors.New("--install cannot be combined with --env")
		}
		if fileFlag.set {
			return options, errors.New("--install cannot be combined with --file")
		}
		if len(remaining) > 0 {
			return options, errors.New("--install cannot be combined with a positional profile")
		}
		return options, nil
	}
	if options.update {
		if profileFlag.set {
			return options, errors.New("--update cannot be combined with --profile")
		}
		if envFlag.set {
			return options, errors.New("--update cannot be combined with --env")
		}
		if fileFlag.set {
			return options, errors.New("--update cannot be combined with --file")
		}
		if options.install {
			return options, errors.New("--update cannot be combined with --install")
		}
		if options.showVersion {
			return options, errors.New("--update cannot be combined with --version")
		}
		if len(remaining) > 0 {
			return options, errors.New("--update cannot be combined with a positional profile")
		}
		return options, nil
	}
	if len(remaining) > 1 {
		return options, fmt.Errorf("unexpected extra arguments: %s", strings.Join(remaining[1:], " "))
	}
	if len(remaining) == 1 {
		positionalProfile := strings.TrimSpace(remaining[0])
		if positionalProfile == "" {
			return options, errors.New("profile cannot be empty")
		}
		if profileFlag.set && positionalProfile != options.profile {
			return options, fmt.Errorf("conflicting profile values: positional %q and flag %q", positionalProfile, options.profile)
		}
		options.profile = positionalProfile
	}

	return options, nil
}

func (a *app) printUsage(w io.Writer) {
	fmt.Fprintln(w, "deploy")
	fmt.Fprintln(w, "  Docker Compose deployment helper with a terminal UI.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  deploy [profile]")
	fmt.Fprintln(w, "  deploy [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -h, --help             Show help and exit")
	fmt.Fprintln(w, "  -i, --install          Install deploy into a user executable path and exit")
	fmt.Fprintln(w, "  -p, --profile NAME     Set COMPOSE_PROFILES (default: dev)")
	fmt.Fprintln(w, "  -u, --update           Update deploy to the latest stable release and exit")
	fmt.Fprintln(w, "  -e, --env PATH         Preselect the env file")
	fmt.Fprintln(w, "  -f, --file PATH        Preselect the compose file")
	fmt.Fprintln(w, "  -v, --version          Show version and exit")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Notes:")
	fmt.Fprintln(w, "  Actions are still selected in the terminal UI.")
	fmt.Fprintln(w, "  The positional profile form remains supported for compatibility.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Examples:")
	fmt.Fprintln(w, "  deploy")
	fmt.Fprintln(w, "  deploy --install")
	fmt.Fprintln(w, "  deploy --update")
	fmt.Fprintln(w, "  deploy prod")
	fmt.Fprintln(w, "  deploy --profile prod")
	fmt.Fprintln(w, "  deploy -e .env.prod -f compose.yml")
	fmt.Fprintln(w, "  deploy --version")
}

func versionString() string {
	return fmt.Sprintf("deploy %s", version)
}

func (a *app) installSelf() error {
	resolvedSource, err := currentExecutablePath()
	if err != nil {
		return err
	}

	target, err := resolveInstallTarget(runtime.GOOS, currentEnvironment(), os.Getenv("PATH"), isDirWritable)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(target.dir, 0o755); err != nil {
		return fmt.Errorf("failed to create install directory %s: %w", target.dir, err)
	}

	installed, err := installExecutable(resolvedSource, target.path, runtime.GOOS)
	if err != nil {
		return err
	}

	if installed {
		fmt.Fprintf(a.stdout, "Installed deploy to %s\n", target.path)
	} else {
		fmt.Fprintf(a.stdout, "deploy is already installed at %s\n", target.path)
	}

	if target.pathInstruction != "" {
		fmt.Fprintf(a.stdout, "Add it to PATH with:\n%s\n", target.pathInstruction)
	}

	return nil
}

func (a *app) selectDeploymentTarget(rawSSHURI string) (deploymentTarget, error) {
	sshURI := strings.TrimSpace(rawSSHURI)
	if sshURI == "" {
		a.msgBox("SSH_URI Not Set", "The selected env file does not set SSH_URI. Only local deployment is allowed.")
		fmt.Fprintln(a.stdout, "Selected deployment method: Locally")
		return deploymentTarget{label: "Locally"}, nil
	}

	normalized, sshTarget, err := normalizeSSHURI(sshURI)
	if err != nil {
		return deploymentTarget{}, fmt.Errorf("invalid SSH_URI %q: %w", sshURI, err)
	}

	selected, err := a.promptSelect(
		"Select Deployment Method",
		"Choose deployment method:",
		[]menuOption{
			{label: sshURI},
			{label: "Locally"},
		},
	)
	if err != nil {
		return deploymentTarget{}, err
	}

	switch selected {
	case sshURI:
		fmt.Fprintf(a.stdout, "Selected deployment method: Pre-configured SSH URI (%s)\n", sshURI)
		return deploymentTarget{
			label:      sshURI,
			dockerHost: normalized,
			sshTarget:  sshTarget,
		}, nil
	case "Locally":
		fmt.Fprintln(a.stdout, "Selected deployment method: Locally")
		return deploymentTarget{label: "Locally"}, nil
	default:
		return deploymentTarget{}, fmt.Errorf("unknown deployment target %q", selected)
	}
}

func (a *app) selectMainAction(ctx composeContext) (string, error) {
	options := []menuOption{
		{label: "Deploy All Services"},
		{label: "Redeploy Service"},
		{label: "Restart Service"},
		{label: "Undeploy Service"},
		{label: "Service Shell"},
		{label: "Service Logs"},
		{label: "Live Service Log Viewer"},
		{label: "Undeploy All Services"},
		{label: "Undeploy All Services (Keep Volumes)"},
		{label: "Create External Networks"},
	}

	if ctx.sshURI == "" {
		options = append(options, menuOption{label: "Host Shell (Unavailable)"})
	}
	if ctx.target.sshTarget != "" {
		options = append(options, menuOption{label: "Host Shell"})
	}

	return a.promptSelect("Main Menu", "Choose an action:", options)
}

func (a *app) executeAction(ctx composeContext, profile, action string) error {
	composeEnv := buildComposeEnv(ctx.envValues, profile, ctx.target.dockerHost)

	switch action {
	case "Deploy All Services":
		fmt.Fprintln(a.stdout, "Deploying all services...")
		if err := a.createExternalNetworks(ctx, composeEnv); err != nil {
			return err
		}
		if err := runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, composeBuildArgs("")...); err != nil {
			return err
		}
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "up", "-d", "--force-recreate", "--remove-orphans")
	case "Redeploy Service":
		service, err := a.selectService(ctx.model)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Redeploying service: %s...\n", service)
		if err := runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, composeBuildArgs(service)...); err != nil {
			return err
		}
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "up", "-d", "--force-recreate", "--remove-orphans", service)
	case "Restart Service":
		service, err := a.selectService(ctx.model)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Restarting service: %s...\n", service)
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "restart", service)
	case "Undeploy Service":
		service, err := a.selectService(ctx.model)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Undeploying service: %s...\n", service)
		if err := runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "stop", service); err != nil {
			fmt.Fprintf(a.stderr, "Warning: failed to stop service %s before removal: %v\n", service, err)
		}
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "rm", "-f", "-v", service)
	case "Service Shell":
		service, err := a.selectService(ctx.model)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Accessing shell for service: %s...\n", service)
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "exec", "-ti", service, "sh")
	case "Service Logs":
		service, err := a.selectService(ctx.model)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Displaying logs for service: %s...\n", service)
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "logs", service)
	case "Live Service Log Viewer":
		service, err := a.selectService(ctx.model)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Following live logs for service: %s (Press Ctrl+C to stop)...\n", service)
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "logs", "--tail", "0", "-f", service)
	case "Undeploy All Services":
		fmt.Fprintln(a.stdout, "Undeploying all services...")
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "down", "-v")
	case "Undeploy All Services (Keep Volumes)":
		fmt.Fprintln(a.stdout, "Undeploying all services without removing volumes...")
		return runDockerCompose(composeEnv, ctx.envFile, ctx.composeFile, true, "down")
	case "Create External Networks":
		return a.createExternalNetworks(ctx, composeEnv)
	case "Host Shell":
		return openHostShell(a.stdin, a.stdout, a.stderr, ctx.target.sshTarget)
	case "Host Shell (Unavailable)":
		a.msgBox("Host Shell Unavailable", "Host Shell requires SSH deployment. This env file does not set SSH_URI, so only local deployment is allowed.")
		return nil
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

func (a *app) createExternalNetworks(ctx composeContext, composeEnv []string) error {
	fmt.Fprintln(a.stdout, "Checking for and creating external networks...")

	names := externalNetworkNames(ctx.model)
	if len(names) == 0 {
		fmt.Fprintf(a.stdout, "No external networks defined in %s.\n", ctx.composeFile)
		return nil
	}

	for _, name := range names {
		fmt.Fprintf(a.stdout, "Attempting to create external network: %s\n", name)
		exists, err := dockerNetworkExists(composeEnv, name)
		if err != nil {
			return err
		}
		if exists {
			fmt.Fprintf(a.stdout, "Network %q already exists. Skipping creation.\n", name)
			continue
		}
		if err := runDockerCommand(composeEnv, true, "network", "create", name); err != nil {
			return err
		}
	}

	return nil
}

func (a *app) selectService(model composeModel) (string, error) {
	services := make([]string, 0, len(model.Services))
	for service := range model.Services {
		services = append(services, service)
	}
	sort.Strings(services)

	if len(services) == 0 {
		return "", errors.New("no services found in compose configuration")
	}

	options := make([]menuOption, 0, len(services))
	for _, service := range services {
		options = append(options, menuOption{label: service})
	}

	return a.promptSelect("Select a Service", "Choose a service:", options)
}

type menuOption struct {
	label string
}

func (a *app) promptSelect(title, prompt string, options []menuOption) (string, error) {
	if a.canUseTUI() {
		return a.promptSelectTUI(title, prompt, options)
	}

	return a.promptSelectFallback(title, prompt, options)
}

func (a *app) promptSelectFallback(title, prompt string, options []menuOption) (string, error) {
	if len(options) == 0 {
		return "", errors.New("no options available")
	}

	for {
		fmt.Fprintf(a.stdout, "\n%s\n%s\n", title, prompt)
		for i, option := range options {
			fmt.Fprintf(a.stdout, "  %d. %s\n", i+1, option.label)
		}
		fmt.Fprintln(a.stdout, "  0. Cancel")
		fmt.Fprint(a.stdout, "> ")

		line, err := a.reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", errSelectionCancelled
			}
			return "", err
		}

		line = strings.TrimSpace(line)
		choice, err := strconv.Atoi(line)
		if err != nil {
			fmt.Fprintln(a.stderr, "Enter a number from the list.")
			continue
		}
		if choice == 0 {
			return "", errSelectionCancelled
		}
		if choice < 1 || choice > len(options) {
			fmt.Fprintln(a.stderr, "Selection out of range.")
			continue
		}

		return options[choice-1].label, nil
	}
}

func (a *app) msgBox(title, message string) {
	if a.canUseTUI() {
		if err := a.msgBoxTUI(title, message); err == nil {
			return
		}
	}

	fmt.Fprintf(a.stderr, "%s: %s\n", title, message)
}

func (a *app) canUseTUI() bool {
	stdoutFile, ok := a.stdout.(*os.File)
	if !ok {
		return false
	}

	return term.IsTerminal(int(a.stdin.Fd())) && term.IsTerminal(int(stdoutFile.Fd()))
}

func (a *app) promptSelectTUI(title, prompt string, options []menuOption) (string, error) {
	if len(options) == 0 {
		return "", errors.New("no options available")
	}

	model := newMenuModel(title, prompt, options)
	program := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithInput(a.stdin),
		tea.WithOutput(a.stdout),
	)

	result, err := program.Run()
	if err != nil {
		return "", err
	}

	finalModel, ok := result.(menuModel)
	if !ok {
		return "", errors.New("failed to read menu result")
	}
	if finalModel.cancelled || finalModel.choice == "" {
		return "", errSelectionCancelled
	}

	return finalModel.choice, nil
}

func (a *app) msgBoxTUI(title, message string) error {
	model := newDialogModel(title, message)
	program := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithInput(a.stdin),
		tea.WithOutput(a.stdout),
	)

	_, err := program.Run()
	return err
}

func (a *app) selectFile(fileType string, defaults, patterns []string) (string, error) {
	files, err := discoverFiles(defaults, patterns)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no %s files found in %s", fileType, mustGetwd())
	}

	options := make([]menuOption, 0, len(files))
	for _, file := range files {
		options = append(options, menuOption{label: file})
	}

	selected, err := a.promptSelect("Select "+fileType+" File", "Choose a "+fileType+" file:", options)
	if err != nil {
		return "", err
	}
	return selected, nil
}

func (a *app) resolveInputFile(preselected, fileType string, defaults, patterns []string) (string, error) {
	if strings.TrimSpace(preselected) == "" {
		return a.selectFile(fileType, defaults, patterns)
	}

	return validateInputFile(preselected, fileType)
}

func validateInputFile(path, fileType string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	info, err := os.Stat(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s file not found: %s", fileType, path)
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s path is a directory, not a file: %s", fileType, path)
	}
	if filepath.IsAbs(cleaned) {
		return cleaned, nil
	}
	return normalizeDisplayPath(cleaned), nil
}

type menuModel struct {
	title     string
	prompt    string
	options   []menuOption
	cursor    int
	choice    string
	cancelled bool
	width     int
	height    int
}

func newMenuModel(title, prompt string, options []menuOption) menuModel {
	return menuModel{
		title:   title,
		prompt:  prompt,
		options: options,
	}
}

func (m menuModel) Init() tea.Cmd {
	return nil
}

func (m menuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.options)-1 {
				m.cursor++
			}
		case "enter":
			if len(m.options) > 0 {
				m.choice = m.options[m.cursor].label
			}
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}

	return m, nil
}

func (m menuModel) View() string {
	lines := []string{
		dialogTitleStyle.Render(m.title),
		"",
		dialogBodyStyle.Render(m.prompt),
		"",
	}

	for i, option := range m.options {
		prefix := "  "
		style := optionStyle
		if i == m.cursor {
			prefix = selectedPrefix
			style = selectedOptionStyle
		}
		lines = append(lines, style.Render(prefix+option.label))
	}

	lines = append(lines, "", helpStyle.Render("↑/↓ or j/k to move • Enter to select • Esc or q to cancel"))
	return renderDialog(m.width, m.height, strings.Join(lines, "\n"))
}

type dialogModel struct {
	title   string
	message string
	width   int
	height  int
}

func newDialogModel(title, message string) dialogModel {
	return dialogModel{title: title, message: message}
}

func (m dialogModel) Init() tea.Cmd {
	return nil
}

func (m dialogModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "enter", "esc", "q", "ctrl+c", " ":
			return m, tea.Quit
		}
	}

	return m, nil
}

func (m dialogModel) View() string {
	lines := []string{
		dialogTitleStyle.Render(m.title),
		"",
		dialogBodyStyle.Render(m.message),
		"",
		helpStyle.Render("Enter, Esc, or q to continue"),
	}
	return renderDialog(m.width, m.height, strings.Join(lines, "\n"))
}

var (
	dialogBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("63")).
				Padding(1, 2)
	dialogTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("230"))
	dialogBodyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))
	optionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("250"))
	selectedOptionStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("86"))
	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("244"))
	selectedPrefix = "› "
)

func renderDialog(width, height int, body string) string {
	box := dialogBorderStyle.Render(body)
	if width <= 0 || height <= 0 {
		return box
	}

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

func discoverFiles(defaults, patterns []string) ([]string, error) {
	seen := map[string]struct{}{}
	files := make([]string, 0)

	for _, candidate := range defaults {
		if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
			normalized := normalizeDisplayPath(candidate)
			if _, ok := seen[normalized]; !ok {
				seen[normalized] = struct{}{}
				files = append(files, normalized)
			}
		}
	}

	discovered := make([]string, 0)
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}

		rel := filepath.ToSlash(strings.TrimPrefix(path, "./"))
		parts := strings.Split(rel, "/")

		if d.IsDir() {
			if len(parts) > 1 {
				return fs.SkipDir
			}
			return nil
		}

		if len(parts) > 2 {
			return nil
		}

		base := filepath.Base(path)
		for _, pattern := range patterns {
			matched, err := filepath.Match(pattern, base)
			if err != nil {
				return err
			}
			if matched {
				discovered = append(discovered, normalizeDisplayPath(path))
				return nil
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(discovered)
	for _, path := range discovered {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}

	return files, nil
}

func normalizeDisplayPath(path string) string {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	if cleaned == "." {
		return "./."
	}
	if strings.HasPrefix(cleaned, "./") {
		return cleaned
	}
	return "./" + cleaned
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

type installTarget struct {
	dir             string
	path            string
	pathInstruction string
}

func resolveInstallTarget(goos string, env map[string]string, pathValue string, canWriteDir func(string) bool) (installTarget, error) {
	fallbackDir, err := fallbackInstallDir(goos, env)
	if err != nil {
		return installTarget{}, err
	}
	fileName := executableName(goos)
	if canWriteDir(fallbackDir) {
		target := installTarget{
			dir:  fallbackDir,
			path: joinPathForOS(goos, fallbackDir, fileName),
		}
		if !pathContainsDir(goos, pathValue, fallbackDir) {
			target.pathInstruction = pathInstruction(goos, env, fallbackDir)
		}
		return target, nil
	}

	for _, candidate := range splitPathListForOS(goos, pathValue) {
		dir := strings.TrimSpace(candidate)
		if dir == "" || !isUserScopedPath(goos, dir, env) {
			continue
		}
		if !canWriteDir(dir) {
			continue
		}
		return installTarget{
			dir:  dir,
			path: joinPathForOS(goos, dir, fileName),
		}, nil
	}

	target := installTarget{
		dir:  fallbackDir,
		path: joinPathForOS(goos, fallbackDir, fileName),
	}
	if !pathContainsDir(goos, pathValue, fallbackDir) {
		target.pathInstruction = pathInstruction(goos, env, fallbackDir)
	}

	return target, nil
}

func currentEnvironment() map[string]string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		values[key] = value
	}

	home, err := os.UserHomeDir()
	if err == nil {
		if values["HOME"] == "" {
			values["HOME"] = home
		}
		if values["USERPROFILE"] == "" {
			values["USERPROFILE"] = home
		}
		if runtime.GOOS == "windows" && values["LOCALAPPDATA"] == "" {
			values["LOCALAPPDATA"] = filepath.Join(home, "AppData", "Local")
		}
	}

	return values
}

func executableName(goos string) string {
	if goos == "windows" {
		return "deploy.exe"
	}
	return "deploy"
}

func splitPathListForOS(goos, value string) []string {
	if value == "" {
		return nil
	}

	separator := ":"
	if goos == "windows" {
		separator = ";"
	}
	return strings.Split(value, separator)
}

func isUserScopedPath(goos, dir string, env map[string]string) bool {
	switch goos {
	case "windows":
		userProfile := strings.TrimSpace(env["USERPROFILE"])
		localAppData := strings.TrimSpace(env["LOCALAPPDATA"])
		return isWithinDirForOS(goos, dir, userProfile) || isWithinDirForOS(goos, dir, localAppData)
	default:
		home := strings.TrimSpace(env["HOME"])
		return isWithinDirForOS(goos, dir, home)
	}
}

func isWithinDirForOS(goos, path, root string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(root) == "" {
		return false
	}

	normalizedPath := normalizePathForOS(goos, path)
	normalizedRoot := normalizePathForOS(goos, root)
	if normalizedPath == normalizedRoot {
		return true
	}

	separator := "/"
	if goos == "windows" {
		separator = "\\"
	}

	if strings.HasSuffix(normalizedRoot, separator) {
		return strings.HasPrefix(normalizedPath, normalizedRoot)
	}
	return strings.HasPrefix(normalizedPath, normalizedRoot+separator)
}

func normalizePathForOS(goos, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	if goos == "windows" {
		replaced := strings.ReplaceAll(trimmed, "/", "\\")
		for strings.Contains(replaced, "\\\\") {
			replaced = strings.ReplaceAll(replaced, "\\\\", "\\")
		}
		if len(replaced) > 3 {
			replaced = strings.TrimRight(replaced, "\\")
		}
		return strings.ToLower(replaced)
	}

	cleaned := filepath.Clean(trimmed)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func joinPathForOS(goos string, parts ...string) string {
	if goos == "windows" {
		cleaned := make([]string, 0, len(parts))
		for _, part := range parts {
			trimmed := strings.Trim(strings.ReplaceAll(part, "/", "\\"), "\\")
			if trimmed != "" {
				cleaned = append(cleaned, trimmed)
			}
		}
		if len(cleaned) == 0 {
			return ""
		}

		first := strings.ReplaceAll(parts[0], "/", "\\")
		prefix := ""
		if strings.HasPrefix(first, "\\\\") {
			prefix = "\\\\"
		} else if len(first) >= 3 && first[1] == ':' && first[2] == '\\' {
			prefix = first[:3]
			cleaned[0] = strings.Trim(strings.TrimPrefix(cleaned[0], first[:2]), "\\")
		}
		if prefix != "" {
			return prefix + strings.Join(cleaned, "\\")
		}
		return strings.Join(cleaned, "\\")
	}
	return filepath.Join(parts...)
}

func fallbackInstallDir(goos string, env map[string]string) (string, error) {
	switch goos {
	case "windows":
		userProfile := strings.TrimSpace(env["USERPROFILE"])
		if userProfile == "" {
			return "", errors.New("could not determine USERPROFILE for installation")
		}
		return joinPathForOS(goos, userProfile, "bin"), nil
	case "darwin":
		home := strings.TrimSpace(env["HOME"])
		if home == "" {
			return "", errors.New("could not determine HOME for installation")
		}
		return joinPathForOS(goos, home, ".local", "bin"), nil
	default:
		if xdgBinHome := strings.TrimSpace(env["XDG_BIN_HOME"]); xdgBinHome != "" {
			return xdgBinHome, nil
		}
		home := strings.TrimSpace(env["HOME"])
		if home == "" {
			return "", errors.New("could not determine HOME for installation")
		}
		return joinPathForOS(goos, home, ".local", "bin"), nil
	}
}

func pathContainsDir(goos, pathValue, dir string) bool {
	normalizedDir := normalizePathForOS(goos, dir)
	for _, entry := range splitPathListForOS(goos, pathValue) {
		if normalizePathForOS(goos, entry) == normalizedDir {
			return true
		}
	}
	return false
}

func pathInstruction(goos string, env map[string]string, dir string) string {
	switch goos {
	case "windows":
		userProfile := strings.TrimSpace(env["USERPROFILE"])
		displayDir := dir
		if userProfile != "" && normalizePathForOS(goos, dir) == normalizePathForOS(goos, joinPathForOS(goos, userProfile, "bin")) {
			displayDir = `%USERPROFILE%\bin`
		}
		return "set PATH=" + displayDir + ";%PATH%"
	default:
		home := strings.TrimSpace(env["HOME"])
		displayDir := dir
		if home != "" && normalizePathForOS(goos, dir) == normalizePathForOS(goos, joinPathForOS(goos, home, ".local", "bin")) {
			displayDir = "$HOME/.local/bin"
		}
		return `export PATH="` + displayDir + `:$PATH"`
	}
}

func isDirWritable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}

	file, err := os.CreateTemp(path, ".deploy-write-check-*")
	if err != nil {
		return false
	}

	name := file.Name()
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(name)
		return false
	}

	return os.Remove(name) == nil
}

func installExecutable(sourcePath, targetPath, goos string) (bool, error) {
	if normalizePathForOS(goos, sourcePath) == normalizePathForOS(goos, targetPath) {
		return false, nil
	}

	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return false, fmt.Errorf("failed to open source executable %s: %w", sourcePath, err)
	}
	defer sourceFile.Close()

	info, err := sourceFile.Stat()
	if err != nil {
		return false, fmt.Errorf("failed to stat source executable %s: %w", sourcePath, err)
	}

	dir := filepath.Dir(targetPath)
	tempFile, err := os.CreateTemp(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("failed to create temporary install file: %w", err)
	}

	tempPath := tempFile.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := io.Copy(tempFile, sourceFile); err != nil {
		tempFile.Close()
		return false, fmt.Errorf("failed to copy executable: %w", err)
	}

	mode := info.Mode().Perm()
	if goos != "windows" {
		mode = 0o755
	}
	if err := tempFile.Chmod(mode); err != nil {
		tempFile.Close()
		return false, fmt.Errorf("failed to set executable permissions: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return false, fmt.Errorf("failed to finalize temporary install file: %w", err)
	}

	if goos == "windows" {
		if err := os.Remove(targetPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("failed to replace existing install: %w", err)
		}
	}

	if err := os.Rename(tempPath, targetPath); err != nil {
		return false, fmt.Errorf("failed to install executable to %s: %w", targetPath, err)
	}

	cleanupTemp = false
	return true, nil
}

func parseDotEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	values := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			return nil, fmt.Errorf("line %d uses shell syntax; only dotenv assignments are supported", lineNumber)
		}
		if strings.Contains(line, "`") || strings.Contains(line, "$(") || strings.Contains(line, "&&") || strings.Contains(line, "||") || strings.Contains(line, ";") {
			return nil, fmt.Errorf("line %d contains shell syntax; only dotenv assignments are supported", lineNumber)
		}

		key, rawValue, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d is not a KEY=VALUE assignment", lineNumber)
		}

		key = strings.TrimSpace(key)
		if !isValidEnvKey(key) {
			return nil, fmt.Errorf("line %d has invalid env key %q", lineNumber, key)
		}

		value, err := parseDotEnvValue(rawValue)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		values[key] = value
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return values, nil
}

func parseDotEnvValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}

	if strings.HasPrefix(value, "\"") {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted value: %w", err)
		}
		return unquoted, nil
	}

	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || !strings.HasSuffix(value, "'") {
			return "", errors.New("invalid single-quoted value")
		}
		return value[1 : len(value)-1], nil
	}

	if idx := strings.Index(value, " #"); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value), nil
}

func isValidEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if i == 0 {
			if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func buildComposeEnv(fileVars map[string]string, profile, dockerHost string) []string {
	blocked := map[string]struct{}{
		"COMPOSE_BAKE":     {},
		"COMPOSE_PROFILES": {},
		"DOCKER_HOST":      {},
	}
	for key := range fileVars {
		blocked[key] = struct{}{}
	}

	env := make([]string, 0, len(os.Environ())+3)
	for _, item := range os.Environ() {
		key, _, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		if _, skip := blocked[key]; skip {
			continue
		}
		env = append(env, item)
	}

	env = append(env, "COMPOSE_BAKE=true")
	env = append(env, "COMPOSE_PROFILES="+profile)
	if dockerHost != "" {
		env = append(env, "DOCKER_HOST="+dockerHost)
	}

	return env
}

func composeBaseArgs(envFile, composeFile string) []string {
	return []string{"compose", "--env-file", envFile, "-f", composeFile}
}

func validateCompose(env []string, envFile, composeFile string) error {
	args := append(composeBaseArgs(envFile, composeFile), "config", "--quiet")
	cmd := exec.Command("docker", args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return fmt.Errorf("docker compose config --quiet failed: %w", err)
		}
		return fmt.Errorf("%s", trimmed)
	}
	return nil
}

func loadComposeModel(env []string, envFile, composeFile string) (composeModel, error) {
	args := append(composeBaseArgs(envFile, composeFile), "config", "--format", "json")
	cmd := exec.Command("docker", args...)
	cmd.Env = env
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return composeModel{}, fmt.Errorf("%s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return composeModel{}, err
	}

	var model composeModel
	if err := json.Unmarshal(output, &model); err != nil {
		return composeModel{}, fmt.Errorf("failed to parse docker compose config output: %w", err)
	}

	return model, nil
}

func composeFileHasTopLevelKey(path, key string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return false, err
	}
	if len(node.Content) == 0 {
		return false, nil
	}

	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return false, nil
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return true, nil
		}
	}

	return false, nil
}

func externalNetworkNames(model composeModel) []string {
	names := make([]string, 0)
	for key, network := range model.Networks {
		if !network.External {
			continue
		}
		name := strings.TrimSpace(network.Name)
		if name == "" {
			name = key
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func composeBuildArgs(service string) []string {
	args := []string{"build", "--no-cache"}
	service = strings.TrimSpace(service)
	if service != "" {
		args = append(args, service)
	}
	return args
}

func runDockerCompose(env []string, envFile, composeFile string, interactive bool, args ...string) error {
	base := composeBaseArgs(envFile, composeFile)
	return runDockerCommand(env, interactive, append(base, args...)...)
}

func runDockerCommand(env []string, interactive bool, args ...string) error {
	fmt.Printf("Executing: docker %s\n", strings.Join(args, " "))

	cmd := exec.Command("docker", args...)
	cmd.Env = env
	if interactive {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		fmt.Print(string(output))
	}
	return err
}

func dockerNetworkExists(env []string, name string) (bool, error) {
	cmd := exec.Command("docker", "network", "inspect", name)
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func normalizeSSHURI(value string) (dockerHost string, sshTarget string, err error) {
	spec, err := parseSSHSpec(value)
	if err != nil {
		return "", "", err
	}

	hostPort := spec.host
	if spec.port != "" {
		hostPort = net.JoinHostPort(spec.host, spec.port)
	}

	return "ssh://" + spec.user + "@" + hostPort, spec.user + "@" + hostPort, nil
}

func parseSSHSpec(value string) (sshSpec, error) {
	if strings.HasPrefix(value, "ssh://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return sshSpec{}, err
		}
		if parsed.User == nil || parsed.User.Username() == "" {
			return sshSpec{}, errors.New("missing SSH user")
		}
		host := parsed.Hostname()
		if host == "" {
			return sshSpec{}, errors.New("missing SSH host")
		}
		port := parsed.Port()
		if port == "" {
			port = "22"
		}
		return sshSpec{
			user: parsed.User.Username(),
			host: host,
			port: port,
		}, nil
	}

	user, hostPort, found := strings.Cut(value, "@")
	if !found || user == "" || hostPort == "" {
		return sshSpec{}, errors.New("expected SSH_URI in user@host or ssh://user@host form")
	}

	host, port := splitHostPort(hostPort)
	if host == "" {
		return sshSpec{}, errors.New("missing SSH host")
	}
	if port == "" {
		port = "22"
	}

	return sshSpec{
		user: user,
		host: host,
		port: port,
	}, nil
}

func splitHostPort(hostPort string) (host, port string) {
	if strings.HasPrefix(hostPort, "[") {
		host, port, err := net.SplitHostPort(hostPort)
		if err == nil {
			return host, port
		}
		return hostPort, ""
	}
	if strings.Count(hostPort, ":") == 1 {
		host, port, err := net.SplitHostPort(hostPort)
		if err == nil {
			return host, port
		}
	}
	return hostPort, ""
}

func openHostShell(stdin *os.File, stdout, stderr io.Writer, target string) error {
	spec, err := parseSSHSpec(target)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Accessing host shell via SSH: %s...\n", target)
	args := buildSystemSSHArgs(spec)
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("ssh executable not found; Host Shell requires a local ssh client")
		}
		return err
	}

	return nil
}

func buildSystemSSHArgs(spec sshSpec) []string {
	args := make([]string, 0, 3)
	if spec.port != "" && spec.port != "22" {
		args = append(args, "-p", spec.port)
	}
	args = append(args, spec.user+"@"+spec.host)
	return args
}

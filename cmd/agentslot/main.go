package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"unicode"

	"github.com/LyleLiu666/agentSlot/internal/scaffold"
)

type runtimeEnvironment struct {
	workingDirectory string
	configDirectory  string
	buildVersion     string
	interactive      bool
}

func main() {
	workingDirectory, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentslot:", err)
		os.Exit(1)
	}
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentslot:", err)
		os.Exit(1)
	}
	inputInfo, _ := os.Stdin.Stat()
	environment := runtimeEnvironment{
		workingDirectory: workingDirectory,
		configDirectory:  configDirectory,
		buildVersion:     executableVersion(),
		interactive:      inputInfo != nil && inputInfo.Mode()&os.ModeCharDevice != 0,
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, environment); err != nil {
		fmt.Fprintln(os.Stderr, "agentslot:", err)
		os.Exit(1)
	}
}

func run(arguments []string, input io.Reader, output, errorOutput io.Writer, environment runtimeEnvironment) error {
	if len(arguments) == 0 {
		return errors.New("usage: agentslot init [flags] TARGET")
	}
	if arguments[0] != "init" {
		return fmt.Errorf("unknown command %q; usage: agentslot init [flags] TARGET", arguments[0])
	}
	flags := flag.NewFlagSet("agentslot init", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	var options scaffold.Options
	flags.StringVar(&options.PresetID, "preset", "", "ComponentCatalog preset: local-coding or minimal-chat")
	flags.StringVar(&options.ModulePath, "module", "", "generated Go module path")
	flags.StringVar(&options.AgentSlotVersion, "agentslot-version", "", "exact AgentSlot semantic version")
	flags.StringVar(&options.ProviderKey, "provider-key", "openai-compatible", "model provider key")
	flags.StringVar(&options.ProviderURL, "provider-url", "https://api.openai.com/v1", "OpenAI-compatible base URL")
	flags.StringVar(&options.ModelID, "model", "gpt-5", "model identifier")
	flags.StringVar(&options.CredentialRef, "credential-ref", "providers/openai-compatible", "non-secret credential reference")
	flags.StringVar(&options.CredentialFile, "credential-file", "", "absolute encrypted credential file path")
	flags.StringVar(&options.CredentialKeyEnvironment, "credential-key-env", "AGENTSLOT_CREDENTIAL_KEY_HEX", "decryption-key environment variable name")
	flags.StringVar(&options.Workspace, "workspace", "", "absolute coding Workspace root")
	flags.StringVar(&options.SessionDirectory, "session-dir", "", "absolute Session directory")
	flags.StringVar(&options.ArtifactDirectory, "artifact-dir", "", "absolute Artifact directory")
	flags.StringVar(&options.ApprovalEnvironment, "approval-env", "AGENTSLOT_APPROVE_EFFECTS", "approval environment variable name")
	flags.BoolVar(&options.WithoutFiles, "without-files", false, "omit file tools")
	flags.BoolVar(&options.WithoutShell, "without-shell", false, "omit Bash")
	flags.BoolVar(&options.WithoutSessionHistory, "without-session-history", false, "omit Session history tool")
	var additions, removals stringList
	flags.Var(&additions, "add-implementation", "add or replace a Catalog implementation; repeatable")
	flags.Var(&removals, "remove-implementation", "remove a selected Catalog implementation; repeatable")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("init requires exactly one TARGET directory")
	}
	target, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		return errors.New("cannot resolve TARGET directory")
	}
	options.TargetDirectory = filepath.Clean(target)
	inputReader := bufio.NewReader(input)
	if options.PresetID == "" {
		options.PresetID, err = selectPreset(inputReader, output, environment.interactive)
		if err != nil {
			return err
		}
	}
	if options.AgentSlotVersion == "" {
		options.AgentSlotVersion = environment.buildVersion
	}
	if options.AgentSlotVersion == "" || options.AgentSlotVersion == "(devel)" {
		return errors.New("development builds require --agentslot-version with an existing exact release")
	}
	name := safeName(filepath.Base(options.TargetDirectory))
	if options.ModulePath == "" {
		options.ModulePath = "example.com/" + name
	}
	if environment.workingDirectory == "" {
		return errors.New("working directory is unavailable")
	}
	if environment.configDirectory == "" {
		return errors.New("user config directory is unavailable")
	}
	if options.Workspace == "" {
		options.Workspace = filepath.Clean(environment.workingDirectory)
	}
	stateRoot := filepath.Join(environment.configDirectory, "agentslot", name)
	if options.CredentialFile == "" {
		options.CredentialFile = filepath.Join(environment.configDirectory, "agentslot", "credentials.enc")
	}
	if options.SessionDirectory == "" {
		options.SessionDirectory = filepath.Join(stateRoot, "sessions")
	}
	if options.ArtifactDirectory == "" {
		options.ArtifactDirectory = filepath.Join(stateRoot, "artifacts")
	}
	options.AddImplementations = additions
	options.RemoveImplementations = removals
	if environment.interactive {
		selection, err := scaffold.Select(options)
		if err != nil {
			return err
		}
		if err := configureInteractively(inputReader, output, &options, selection); err != nil {
			return err
		}
	}
	result, err := scaffold.Generate(options)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "generated %s preset at %s (%d files)\n", result.Preset.ID, options.TargetDirectory, len(result.Files))
	for _, adjustment := range result.Adjustments {
		fmt.Fprintf(output, "  %s\n", adjustment)
	}
	return nil
}

func selectPreset(input *bufio.Reader, output io.Writer, interactive bool) (string, error) {
	if !interactive {
		return "local-coding", nil
	}
	if _, err := fmt.Fprint(output, "Select preset: [1] local-coding (default), [2] minimal-chat: "); err != nil {
		return "", err
	}
	selection, err := readLine(input)
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(selection) {
	case "", "1", "local-coding":
		return "local-coding", nil
	case "2", "minimal-chat":
		return "minimal-chat", nil
	default:
		return "", fmt.Errorf("unknown preset selection %q", selection)
	}
}

func configureInteractively(input *bufio.Reader, output io.Writer, options *scaffold.Options, selection scaffold.Selection) error {
	for _, implementation := range selection.Implementations {
		if implementation.ComponentID != "tool" {
			continue
		}
		keep, err := promptBool(input, output, "Install "+implementation.ID, true)
		if err != nil {
			return err
		}
		if !keep {
			options.RemoveImplementations = append(options.RemoveImplementations, implementation.ID)
		}
	}
	selection, err := scaffold.Select(*options)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{})
	for _, implementation := range selection.Implementations {
		for _, field := range implementation.Configuration {
			if _, duplicate := seen[field.Key]; duplicate {
				continue
			}
			seen[field.Key] = struct{}{}
			value, ok := configurationValue(options, field.Key)
			if !ok {
				return fmt.Errorf("agentslot init: unsupported Catalog configuration field %q", field.Key)
			}
			updated, err := prompt(input, output, field.Description, *value)
			if err != nil {
				return err
			}
			*value = updated
		}
	}
	return nil
}

func configurationValue(options *scaffold.Options, key string) (*string, bool) {
	values := map[string]*string{
		"provider-key": &options.ProviderKey, "provider-url": &options.ProviderURL,
		"model-id": &options.ModelID, "credential-ref": &options.CredentialRef,
		"session-directory": &options.SessionDirectory, "credential-file": &options.CredentialFile,
		"credential-key-environment": &options.CredentialKeyEnvironment,
		"workspace":                  &options.Workspace, "artifact-directory": &options.ArtifactDirectory,
		"approval-environment": &options.ApprovalEnvironment,
	}
	value, ok := values[key]
	return value, ok
}

func prompt(input *bufio.Reader, output io.Writer, label, defaultValue string) (string, error) {
	if _, err := fmt.Fprintf(output, "%s [%s]: ", label, defaultValue); err != nil {
		return "", err
	}
	value, err := readLine(input)
	if err != nil {
		return "", err
	}
	if value == "" {
		return defaultValue, nil
	}
	return value, nil
}

func promptBool(input *bufio.Reader, output io.Writer, label string, defaultValue bool) (bool, error) {
	defaultLabel := "y/N"
	if defaultValue {
		defaultLabel = "Y/n"
	}
	if _, err := fmt.Fprintf(output, "%s [%s]: ", label, defaultLabel); err != nil {
		return false, err
	}
	value, err := readLine(input)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(value) {
	case "":
		return defaultValue, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid yes/no answer %q", value)
	}
}

func readLine(input *bufio.Reader) (string, error) {
	value, err := input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("cannot read terminal selection")
	}
	return strings.TrimSpace(value), nil
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("implementation ID cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func safeName(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	lastDash := false
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '.' || character == '_' {
			result.WriteRune(character)
			lastDash = false
			continue
		}
		if !lastDash {
			result.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(result.String(), "-.")
	if name == "" {
		return "generated-agent"
	}
	return name
}

func executableVersion() string {
	information, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	return information.Main.Version
}

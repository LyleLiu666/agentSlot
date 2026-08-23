package scaffold

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/LyleLiu666/agentSlot/componentcatalog"
)

type Options struct {
	TargetDirectory          string
	ModulePath               string
	AgentSlotVersion         string
	PresetID                 string
	ProviderKey              string
	ProviderURL              string
	ModelID                  string
	CredentialRef            string
	CredentialFile           string
	CredentialKeyEnvironment string
	Workspace                string
	SessionDirectory         string
	ArtifactDirectory        string
	ApprovalEnvironment      string
	WithoutFiles             bool
	WithoutShell             bool
	WithoutSessionHistory    bool
	AddImplementations       []string
	RemoveImplementations    []string
}

type File struct {
	Name string
	Mode os.FileMode
	Data []byte
}

type Result struct {
	Preset          componentcatalog.Preset
	Implementations []componentcatalog.Implementation
	Adjustments     []string
	Files           []File
}

type Selection struct {
	Preset          componentcatalog.Preset
	Implementations []componentcatalog.Implementation
	Adjustments     []string
}

var (
	modulePathPattern      = regexp.MustCompile(`^[A-Za-z0-9._~/-]+$`)
	versionPattern         = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func Render(options Options) (Result, error) {
	catalog := componentcatalog.Standard()
	if err := catalog.Validate(); err != nil {
		return Result{}, err
	}
	if err := validateOptions(options); err != nil {
		return Result{}, err
	}
	selection, err := selectFromCatalog(catalog, options)
	if err != nil {
		return Result{}, err
	}
	preset := selection.Preset
	selected := preset.Implementations
	implementations := selection.Implementations
	adjustments := selection.Adjustments
	toolKeys := preset.ToolKeys
	data := templateData{Options: options, Preset: preset}
	data.Workspace = contains(selected, "workspace.local")
	data.Artifact = contains(selected, "artifact.file")
	data.Files = contains(selected, "tool.files")
	data.Shell = contains(selected, "tool.bash")
	data.History = contains(selected, "tool.session-history")
	data.Policy = contains(selected, "policy.tool-rules")
	data.Approval = contains(selected, "approval.configured")
	data.ImplementationIDs = strings.Join(selected, ", ")
	data.ProfileRequirements = profileRequirements(catalog)
	if err := validateSelectedOptions(options, data); err != nil {
		return Result{}, err
	}
	data.ToolKeys = quoted(toolKeys)
	mainSource, err := executeGoTemplate(mainTemplate, data)
	if err != nil {
		return Result{}, err
	}
	testSource, err := executeGoTemplate(testTemplate, data)
	if err != nil {
		return Result{}, err
	}
	readme, err := executeTemplate(readmeTemplate, data)
	if err != nil {
		return Result{}, err
	}
	goMod := []byte(fmt.Sprintf("module %s\n\ngo 1.25.0\n\nrequire github.com/LyleLiu666/agentSlot %s\n", options.ModulePath, options.AgentSlotVersion))
	return Result{Preset: preset, Implementations: implementations, Adjustments: adjustments, Files: []File{
		{Name: "go.mod", Mode: 0o644, Data: goMod},
		{Name: "main.go", Mode: 0o644, Data: mainSource},
		{Name: "main_test.go", Mode: 0o644, Data: testSource},
		{Name: "README.md", Mode: 0o644, Data: readme},
	}}, nil
}

// Select resolves one preset and explicit implementation edits using only the
// standard ComponentCatalog. It does not inspect or validate configuration
// values, so terminal clients can use the returned metadata to collect them.
func Select(options Options) (Selection, error) {
	catalog := componentcatalog.Standard()
	if err := catalog.Validate(); err != nil {
		return Selection{}, err
	}
	return selectFromCatalog(catalog, options)
}

func selectFromCatalog(catalog componentcatalog.Catalog, options Options) (Selection, error) {
	preset, ok := catalog.LookupPreset(options.PresetID)
	if !ok {
		return Selection{}, fmt.Errorf("agentslot init: unknown preset %q", options.PresetID)
	}
	selected := slices.Clone(preset.Implementations)
	toolKeys := slices.Clone(preset.ToolKeys)
	removals := slices.Clone(options.RemoveImplementations)
	if options.WithoutFiles {
		removals = append(removals, "tool.files")
	}
	if options.WithoutShell {
		removals = append(removals, "tool.bash")
	}
	if options.WithoutSessionHistory {
		removals = append(removals, "tool.session-history")
	}
	removed := make(map[string]struct{}, len(removals))
	for _, id := range removals {
		implementation, ok := catalog.LookupImplementation(id)
		if !ok {
			return Selection{}, fmt.Errorf("agentslot init: unknown implementation %q", id)
		}
		if !contains(selected, id) {
			return Selection{}, fmt.Errorf("agentslot init: implementation %q is not selected", id)
		}
		selected = remove(selected, id)
		toolKeys = remove(toolKeys, implementation.ToolKeys...)
		removed[id] = struct{}{}
	}
	adjustments := make([]string, 0)
	for _, id := range options.AddImplementations {
		var err error
		selected, toolKeys, adjustments, err = addImplementation(catalog, selected, toolKeys, adjustments, removed, id)
		if err != nil {
			return Selection{}, err
		}
	}
	preset.Implementations = selected
	preset.ToolKeys = toolKeys
	validationCatalog := catalog
	validationCatalog.Presets = []componentcatalog.Preset{preset}
	if err := validationCatalog.Validate(); err != nil {
		return Selection{}, fmt.Errorf("agentslot init: invalid selection: %w", err)
	}
	implementations := make([]componentcatalog.Implementation, 0, len(selected))
	for _, id := range selected {
		implementation, ok := catalog.LookupImplementation(id)
		if !ok || !implementation.Available {
			return Selection{}, fmt.Errorf("agentslot init: implementation %q is unavailable", id)
		}
		implementations = append(implementations, implementation)
	}
	return Selection{Preset: preset, Implementations: implementations, Adjustments: adjustments}, nil
}

func Generate(options Options) (Result, error) {
	result, err := Render(options)
	if err != nil {
		return Result{}, err
	}
	target := options.TargetDirectory
	if _, err := os.Lstat(target); err == nil {
		return Result{}, errors.New("agentslot init: target directory already exists; no files were changed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, errors.New("agentslot init: cannot inspect target directory")
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Result{}, errors.New("agentslot init: cannot create target parent")
	}
	temporary, err := os.MkdirTemp(parent, ".agentslot-init-*")
	if err != nil {
		return Result{}, errors.New("agentslot init: cannot stage generated project")
	}
	defer os.RemoveAll(temporary)
	for _, generated := range result.Files {
		if err := os.WriteFile(filepath.Join(temporary, generated.Name), generated.Data, generated.Mode); err != nil {
			return Result{}, errors.New("agentslot init: cannot stage generated file")
		}
	}
	if err := os.Rename(temporary, target); err != nil {
		return Result{}, errors.New("agentslot init: cannot commit generated project")
	}
	return result, nil
}

func validateOptions(options Options) error {
	if options.TargetDirectory == "" || !filepath.IsAbs(options.TargetDirectory) || filepath.Clean(options.TargetDirectory) != options.TargetDirectory {
		return errors.New("agentslot init: target directory must be absolute and clean")
	}
	if options.ModulePath == "" || !modulePathPattern.MatchString(options.ModulePath) || strings.Contains(options.ModulePath, "//") {
		return errors.New("agentslot init: invalid Go module path")
	}
	if !versionPattern.MatchString(options.AgentSlotVersion) {
		return errors.New("agentslot init: an exact semantic AgentSlot version is required")
	}
	if options.PresetID == "" || options.ProviderKey == "" || options.ModelID == "" || options.CredentialRef == "" {
		return errors.New("agentslot init: preset, provider, model, and CredentialRef are required")
	}
	if !environmentNamePattern.MatchString(options.CredentialKeyEnvironment) {
		return errors.New("agentslot init: credential-key environment name must be valid")
	}
	parsed, err := url.Parse(options.ProviderURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("agentslot init: provider URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	for _, path := range []string{options.CredentialFile, options.SessionDirectory} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("agentslot init: credential and Session paths must be absolute and clean")
		}
	}
	return nil
}

func validateSelectedOptions(options Options, data templateData) error {
	if data.Approval && !environmentNamePattern.MatchString(options.ApprovalEnvironment) {
		return errors.New("agentslot init: approval environment name must be valid")
	}
	if data.Workspace && (options.Workspace == "" || !filepath.IsAbs(options.Workspace) || filepath.Clean(options.Workspace) != options.Workspace) {
		return errors.New("agentslot init: selected local Workspace path must be absolute and clean")
	}
	if data.Artifact && (options.ArtifactDirectory == "" || !filepath.IsAbs(options.ArtifactDirectory) || filepath.Clean(options.ArtifactDirectory) != options.ArtifactDirectory) {
		return errors.New("agentslot init: selected Artifact path must be absolute and clean")
	}
	if data.Workspace {
		protected := []string{options.CredentialFile, options.SessionDirectory}
		if data.Artifact {
			protected = append(protected, options.ArtifactDirectory)
		}
		for _, path := range protected {
			if pathWithin(path, options.Workspace) {
				return errors.New("agentslot init: credentials, Sessions, and Artifacts must be outside the Workspace boundary")
			}
		}
	}
	return nil
}

func addImplementation(catalog componentcatalog.Catalog, selected, toolKeys, adjustments []string, removed map[string]struct{}, id string) ([]string, []string, []string, error) {
	implementation, ok := catalog.LookupImplementation(id)
	if !ok {
		return nil, nil, nil, fmt.Errorf("agentslot init: unknown implementation %q", id)
	}
	if !implementation.Available {
		return nil, nil, nil, fmt.Errorf("agentslot init: implementation %q is unavailable: %s", id, implementation.UnavailableReason)
	}
	if contains(selected, id) {
		return selected, toolKeys, adjustments, nil
	}
	component, _ := catalog.Lookup(implementation.ComponentID)
	if component.Kind == componentcatalog.KindOne {
		for _, existingID := range slices.Clone(selected) {
			existing, _ := catalog.LookupImplementation(existingID)
			if existing.ComponentID == implementation.ComponentID {
				selected = remove(selected, existingID)
				toolKeys = remove(toolKeys, existing.ToolKeys...)
				adjustments = append(adjustments, fmt.Sprintf("replaced %s with %s for %s", existingID, id, implementation.ComponentID))
			}
		}
	}
	selected = append(selected, id)
	for _, key := range implementation.ToolKeys {
		if !contains(toolKeys, key) {
			toolKeys = append(toolKeys, key)
		}
	}
	for _, dependency := range implementation.Dependencies {
		if selectedComponent(catalog, selected, dependency) {
			continue
		}
		candidates := make([]string, 0, 1)
		for _, candidate := range catalog.Implementations {
			if candidate.Available && candidate.ComponentID == dependency {
				if _, forbidden := removed[candidate.ID]; !forbidden {
					candidates = append(candidates, candidate.ID)
				}
			}
		}
		if len(candidates) != 1 {
			return nil, nil, nil, fmt.Errorf("agentslot init: %q requires %q; select exactly one available implementation", id, dependency)
		}
		adjustments = append(adjustments, fmt.Sprintf("added %s because %s requires %s", candidates[0], id, dependency))
		var err error
		selected, toolKeys, adjustments, err = addImplementation(catalog, selected, toolKeys, adjustments, removed, candidates[0])
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return selected, toolKeys, adjustments, nil
}

func selectedComponent(catalog componentcatalog.Catalog, selected []string, componentID string) bool {
	for _, id := range selected {
		implementation, _ := catalog.LookupImplementation(id)
		if implementation.ComponentID == componentID {
			return true
		}
	}
	return false
}

func profileRequirements(catalog componentcatalog.Catalog) string {
	values := make([]string, 0, 5)
	for _, component := range catalog.Components {
		for _, requirement := range component.Profiles {
			if requirement.Name == "standard-agent" {
				values = append(values, fmt.Sprintf("%s >= %d", component.ID, requirement.Minimum))
			}
		}
	}
	return strings.Join(values, ", ")
}

func pathWithin(candidate, root string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func remove(values []string, removed ...string) []string {
	set := make(map[string]struct{}, len(removed))
	for _, value := range removed {
		set[value] = struct{}{}
	}
	return slices.DeleteFunc(values, func(value string) bool { _, ok := set[value]; return ok })
}

func contains(values []string, target string) bool { return slices.Contains(values, target) }

func quoted(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = fmt.Sprintf("%q", value)
	}
	return strings.Join(quoted, ", ")
}

type templateData struct {
	Options             Options
	Preset              componentcatalog.Preset
	Workspace           bool
	Artifact            bool
	Files               bool
	Shell               bool
	History             bool
	Policy              bool
	Approval            bool
	ToolKeys            string
	ImplementationIDs   string
	ProfileRequirements string
}

func executeGoTemplate(source string, data templateData) ([]byte, error) {
	rendered, err := executeTemplate(source, data)
	if err != nil {
		return nil, err
	}
	formatted, err := format.Source(rendered)
	if err != nil {
		return nil, fmt.Errorf("agentslot init: format generated Go: %w", err)
	}
	return formatted, nil
}

func executeTemplate(source string, data templateData) ([]byte, error) {
	parsed, err := template.New("scaffold").Parse(source)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := parsed.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

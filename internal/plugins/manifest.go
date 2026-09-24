// Package plugins installs, runs and serves RepeaterTastic plugins: separate programs that
// talk to the daemon over the Plugin API (proto/plugin/v1). See docs/plugins.md.
package plugins

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the Plugin API version this daemon speaks.
const APIVersion = 1

// ManifestFile is the manifest's name at the root of a bundle.
const ManifestFile = "plugin.yaml"

// Permissions a plugin can ask for, with the sentence the GUI shows for each.
var Permissions = map[string]string{
	"packets.read":    "See every packet the radios hear and send, decoded where RepeaterTastic can read it",
	"nodes.read":      "See the node database",
	"messages.read":   "Read text messages to and from each radio's relay persona",
	"messages.send":   "Send text messages from each radio's relay persona",
	"traceroute.send": "Send traceroutes from the identity chosen in the plugin's settings (or the radio's relay persona)",
	"status.read":     "See how the radios are doing: airtime, noise floor, channel use and the packet counters",
	"sensors.publish": "Give readings to the host's push sensors, which the identities they are attached to publish as their own",
}

// Manifest is plugin.yaml.
type Manifest struct {
	ID          string    `yaml:"id" json:"id"`
	Name        string    `yaml:"name" json:"name"`
	Version     string    `yaml:"version" json:"version"`
	API         int       `yaml:"api" json:"api"`
	Description string    `yaml:"description" json:"description"`
	Author      string    `yaml:"author" json:"author"`
	Homepage    string    `yaml:"homepage" json:"homepage"`
	License     string    `yaml:"license" json:"license"`
	Logo        string    `yaml:"logo" json:"logo"` // bundle path to a PNG, SVG or WebP
	Permissions []string  `yaml:"permissions" json:"permissions"`
	Network     []string  `yaml:"network" json:"network"` // hosts the plugin talks to, shown before enabling
	Settings    []Setting `yaml:"settings" json:"settings"`
	Run         Run       `yaml:"run" json:"run"`
	UI          UI        `yaml:"ui" json:"ui"`
}

// Run says how RepeaterTastic starts the plugin.
type Run struct {
	Managed *Managed `yaml:"managed" json:"managed,omitempty"`
}

// Managed says how RepeaterTastic runs a plugin: Exec (a bundle path; {os} and {arch} are replaced, e.g.
// bin/my-plugin-{os}-{arch}) and keeps it running while the plugin is enabled.
type Managed struct {
	Exec string   `yaml:"exec" json:"exec"`
	Args []string `yaml:"args" json:"args,omitempty"`
}

// UI is the plugin's optional GUI panel: Panel is a bundle path to an HTML file shown in a
// sandboxed frame on the plugin's page.
type UI struct {
	Panel string `yaml:"panel" json:"panel,omitempty"`
}

// Setting is one field of the plugin's settings form.
type Setting struct {
	Key   string `yaml:"key" json:"key"`
	Label string `yaml:"label" json:"label"`
	// string, secret, url, bool, int, number, select, multiselect (a list from options), radios
	// (a list of the site's radio IDs; empty usually means every radio), identities (a list of
	// the site's identities' node IDs) or nodes (a list of node IDs picked from the nodes the
	// site has heard)
	Type        string   `yaml:"type" json:"type"`
	Help        string   `yaml:"help" json:"help,omitempty"`
	Required    bool     `yaml:"required" json:"required,omitempty"`
	Default     any      `yaml:"default" json:"default,omitempty"`
	Options     []string `yaml:"options" json:"options,omitempty"` // select, multiselect
	Placeholder string   `yaml:"placeholder" json:"placeholder,omitempty"`
}

var (
	idPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,39}$`)
	settingPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	settingTypes    = []string{"string", "secret", "url", "bool", "int", "number", "select", "multiselect", "radios", "identities", "nodes"}
	logoExtensions  = []string{".png", ".svg", ".webp"}
	errNoManagedRun = errors.New("plugin has no run.managed.exec")
)

// ParseManifest reads and checks a manifest.
func ParseManifest(b []byte) (*Manifest, error) {
	m := &Manifest{}
	if err := yaml.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	return m, m.Validate()
}

// Validate checks the fields that don't depend on the bundle's files.
func (m *Manifest) Validate() error {
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("id %q must be 2-40 lowercase letters, digits and dashes", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		m.Name = m.ID
	}
	if m.API != APIVersion {
		return fmt.Errorf("plugin needs Plugin API %d; this RepeaterTastic speaks %d", m.API, APIVersion)
	}
	for _, p := range m.Permissions {
		if _, ok := Permissions[p]; !ok {
			return fmt.Errorf("unknown permission %q", p)
		}
	}
	if err := m.validateSettings(); err != nil {
		return err
	}
	return m.validatePaths()
}

// validateSettings checks the settings schema and fills in default types and labels.
func (m *Manifest) validateSettings() error {
	keys := map[string]bool{}
	for i, s := range m.Settings {
		if !settingPattern.MatchString(s.Key) || keys[s.Key] {
			return fmt.Errorf("setting %d: key %q must be unique, lowercase letters, digits and underscores", i+1, s.Key)
		}
		keys[s.Key] = true
		if s.Type == "" {
			m.Settings[i].Type = "string"
		} else if !slices.Contains(settingTypes, s.Type) {
			return fmt.Errorf("setting %s: type %q must be one of %s", s.Key, s.Type, strings.Join(settingTypes, ", "))
		}
		if (s.Type == "select" || s.Type == "multiselect") && len(s.Options) == 0 {
			return fmt.Errorf("setting %s: a %s needs options", s.Key, s.Type)
		}
		if s.Label == "" {
			m.Settings[i].Label = s.Key
		}
	}
	return nil
}

// validatePaths checks the logo, panel and program paths stay inside the bundle.
func (m *Manifest) validatePaths() error {
	for _, f := range []struct{ name, p string }{{"logo", m.Logo}, {"ui.panel", m.UI.Panel}} {
		if f.p != "" && !localPath(f.p) {
			return fmt.Errorf("%s %q must be a relative path inside the bundle", f.name, f.p)
		}
	}
	if m.Logo != "" && !slices.Contains(logoExtensions, strings.ToLower(path.Ext(m.Logo))) {
		return fmt.Errorf("logo must be a PNG, SVG or WebP file")
	}
	if m.Run.Managed != nil && !localPath(m.Run.Managed.Exec) {
		return fmt.Errorf("run.managed.exec %q must be a relative path inside the bundle", m.Run.Managed.Exec)
	}
	return nil
}

// ExecPath is the managed executable for this host, relative to the bundle.
func (m *Manifest) ExecPath() (string, error) {
	if m.Run.Managed == nil || m.Run.Managed.Exec == "" {
		return "", errNoManagedRun
	}
	return strings.NewReplacer("{os}", runtime.GOOS, "{arch}", runtime.GOARCH).Replace(m.Run.Managed.Exec), nil
}

// checkFiles checks the files the manifest names exist in the extracted bundle at dir.
func (m *Manifest) checkFiles(dir string) error {
	if m.Logo != "" {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(m.Logo))); err != nil {
			return fmt.Errorf("logo %s is missing from the bundle", m.Logo)
		}
	}
	if m.UI.Panel != "" {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(m.UI.Panel))); err != nil {
			return fmt.Errorf("ui.panel %s is missing from the bundle", m.UI.Panel)
		}
	}
	if m.Run.Managed == nil {
		return nil
	}
	exe, _ := m.ExecPath()
	st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(exe)))
	if err != nil || st.IsDir() {
		return fmt.Errorf("this plugin has no program for %s/%s (%s)", runtime.GOOS, runtime.GOARCH, exe)
	}
	return nil
}

// localPath: a clean relative slash path that stays inside its root.
func localPath(p string) bool {
	return p != "" && !strings.Contains(p, `\`) && filepath.IsLocal(filepath.FromSlash(p)) && path.Clean(p) == p
}

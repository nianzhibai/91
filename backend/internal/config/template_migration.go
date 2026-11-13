package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/nianzhibai/91/backend"
	"gopkg.in/yaml.v3"
)

// SyncTemplate rebuilds the document from the release template, carrying over
// existing values only at paths still present in that template. Comments, field
// order and missing values come from config.example.yaml. Call this after legacy
// imports so template defaults cannot mask settings still stored in SQLite.
func (m *Manager) SyncTemplate() (bool, error) {
	if m == nil {
		return false, errors.New("configuration manager is unavailable")
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	data, err := os.ReadFile(m.path)
	if err != nil {
		return false, fmt.Errorf("read config for template migration: %w", err)
	}
	if err := validateAdminConfigRemoved(data); err != nil {
		return false, err
	}
	parsed, err := Parse(data)
	if err != nil {
		return false, err
	}
	// Decode aliases and merge keys before copying values; the new document has
	// the template's structure and no dependency on anchors in retired fields.
	var values map[string]yaml.Node
	if err := yaml.Unmarshal(data, &values); err != nil {
		return false, err
	}
	var template yaml.Node
	if err := yaml.Unmarshal(backend.ConfigTemplate(), &template); err != nil {
		return false, fmt.Errorf("parse bundled config template: %w", err)
	}
	if err := copyConfigValues(template.Content[0], values); err != nil {
		return false, fmt.Errorf("copy config values: %w", err)
	}
	// Old layouts are validated and converted by Parse before their retired
	// fields are removed. Preserve that root instead of taking the default.
	setScalarValue(ensureMappingValue(template.Content[0], "storage"), "data_dir", parsed.Storage.DataDir)
	// Parse also disables Telegram sections left by the retired Bot API service.
	// Keep that state: the template's empty credentials would fail validation.
	if !parsed.Telegram.Enabled {
		telegram, _ := mappingValue(template.Content[0], "telegram")
		if enabled, ok := mappingValue(telegram, "enabled"); ok {
			enabled.Kind, enabled.Tag, enabled.Value = yaml.ScalarNode, "!!bool", "false"
		}
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&template); err != nil {
		_ = encoder.Close()
		return false, fmt.Errorf("encode config template: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return false, err
	}
	migrated, err := Parse(out.Bytes())
	if err != nil {
		return false, fmt.Errorf("validate migrated config: %w", err)
	}
	if bytes.Equal(data, out.Bytes()) {
		return false, nil
	}
	mode := configFileMode(m.path)
	if err := writeFileAtomically(m.path, out.Bytes(), mode); err != nil {
		return false, err
	}
	if _, err := m.publishLocked(migrated, configVersion(out.Bytes())); err != nil {
		return false, errors.Join(err, writeFileAtomically(m.path, data, mode))
	}
	return true, nil
}

func copyConfigValues(template *yaml.Node, values map[string]yaml.Node) error {
	for i := 0; i+1 < len(template.Content); i += 2 {
		key, target := template.Content[i], template.Content[i+1]
		existing, exists := values[key.Value]
		if !exists {
			continue
		}
		if target.Kind == yaml.MappingNode {
			// An omitted/null section has no individual settings to retain.
			var section map[string]yaml.Node
			if err := existing.Decode(&section); err != nil {
				return err
			}
			if err := copyConfigValues(target, section); err != nil {
				return err
			}
			continue
		}
		replacement := copyConfigValue(&existing)
		if replacement.Kind == target.Kind && replacement.Tag == target.Tag {
			replacement.Style = target.Style
		}
		replacement.HeadComment = target.HeadComment
		replacement.LineComment = target.LineComment
		replacement.FootComment = target.FootComment
		*target = *replacement
	}
	return nil
}

// Keep scalar text intact (including strings that look like dates or numbers),
// expand aliases, and leave all comments to the template.
func copyConfigValue(source *yaml.Node) *yaml.Node {
	if source.Kind == yaml.AliasNode {
		return copyConfigValue(source.Alias)
	}
	copy := *source
	copy.Anchor = ""
	copy.HeadComment, copy.LineComment, copy.FootComment = "", "", ""
	copy.Content = nil
	for _, child := range source.Content {
		copy.Content = append(copy.Content, copyConfigValue(child))
	}
	return &copy
}

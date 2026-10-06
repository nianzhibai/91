package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const configWatchInterval = time.Second

var ErrVersionConflict = errors.New("config.yaml changed since it was loaded")

// LiveSettings is the subset of config.yaml that the running process can
// safely apply without rebuilding its long-lived dependencies.
type LiveSettings struct {
	TelegramEnabled        bool   `json:"telegramEnabled"`
	PreviewEnabled         bool   `json:"previewEnabled"`
	ThumbnailConcurrency   int    `json:"thumbnailConcurrency"`
	FingerprintConcurrency int    `json:"fingerprintConcurrency"`
	NightlyDisabled        bool   `json:"nightlyDisabled"`
	NightlyStartTime       string `json:"nightlyStartTime"`
	NightlyTimezone        string `json:"nightlyTimezone"`
	PreviewConcurrency     int    `json:"previewConcurrency"`
}

// LegacyRuntimeSettings carries values written by the short-lived SQLite
// settings implementation. They are consulted only when config.yaml does not
// yet contain the corresponding field.
type LegacyRuntimeSettings struct {
	NightlyStartTime *string
}

type SaveResult struct {
	Version         string       `json:"version"`
	RestartRequired bool         `json:"restartRequired"`
	Settings        LiveSettings `json:"settings"`
}

// Manager owns all config.yaml writes and the in-memory live snapshot. The
// file remains the durable source of truth; the snapshot is only a validated,
// concurrency-safe projection for runtime consumers.
type Manager struct {
	path string

	updateMu sync.Mutex
	mu       sync.RWMutex
	current  *Config
	// observedVersion also records an invalid external revision so the watcher
	// reports it once instead of logging the same rejected bytes every second.
	observedVersion string
	apply           func(LiveSettings) error

	telegramDeployment    telegramDeployment
	telegramDeploymentErr error
}

func NewManager(path string) (*Manager, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	parsed, err := Parse(data)
	if err != nil {
		return nil, err
	}
	version := configVersion(data)
	m := &Manager{
		path:            path,
		current:         parsed,
		observedVersion: version,
	}
	// Telegram is optional. Keep deployment errors for its status/probe rather
	// than prevent an otherwise configured website from starting.
	_ = m.LoadTelegramCompose(filepath.Join(filepath.Dir(path), "telegram.yml"))
	return m, nil
}

func DefaultLiveSettings() LiveSettings {
	return LiveSettings{
		PreviewEnabled:         true,
		NightlyDisabled:        DefaultNightlyDisabled,
		NightlyStartTime:       DefaultNightlyStartTime,
		NightlyTimezone:        DefaultNightlyTimezone,
		PreviewConcurrency:     DefaultGenerationConcurrency,
		ThumbnailConcurrency:   DefaultGenerationConcurrency,
		FingerprintConcurrency: DefaultGenerationConcurrency,
	}
}

func liveSettingsFromConfig(cfg *Config) LiveSettings {
	if cfg == nil {
		return DefaultLiveSettings()
	}
	return LiveSettings{
		TelegramEnabled:        cfg.Telegram.Enabled,
		PreviewEnabled:         cfg.Preview.Enabled,
		NightlyDisabled:        cfg.Nightly.Disabled,
		NightlyStartTime:       cfg.Nightly.StartTime,
		NightlyTimezone:        cfg.Nightly.Timezone,
		PreviewConcurrency:     cfg.Generation.PreviewConcurrency,
		ThumbnailConcurrency:   cfg.Generation.ThumbnailConcurrency,
		FingerprintConcurrency: cfg.Generation.FingerprintConcurrency,
	}
}

func (m *Manager) LiveSettings() LiveSettings {
	if m == nil {
		return DefaultLiveSettings()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return liveSettingsFromConfig(m.current)
}

// SetApply installs the callback used to propagate hot-reloadable fields. It
// immediately supplies the current snapshot, which prevents consumers from
// starting with a stale default when wiring order changes.
func (m *Manager) SetApply(apply func(LiveSettings) error) error {
	if m == nil {
		return errors.New("configuration manager is unavailable")
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	m.mu.RLock()
	settings := liveSettingsFromConfig(m.current)
	m.mu.RUnlock()
	if apply != nil {
		if err := apply(settings); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.apply = apply
	m.mu.Unlock()
	return nil
}

// ReadYAML returns the bytes currently on disk and their content version.
func (m *Manager) ReadYAML() ([]byte, string, error) {
	if m == nil {
		return nil, "", errors.New("configuration manager is unavailable")
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return nil, "", fmt.Errorf("read config: %w", err)
	}
	if err := validateAdminConfigRemoved(data); err != nil {
		return nil, "", err
	}
	return data, configVersion(data), nil
}

// ReplaceYAML validates with the same parser used at startup, rejects a stale
// expected version, atomically replaces the file, then publishes live values.
func (m *Manager) ReplaceYAML(data []byte, expectedVersion string) (SaveResult, error) {
	if m == nil {
		return SaveResult{}, errors.New("configuration manager is unavailable")
	}
	if err := validateAdminConfigRemoved(data); err != nil {
		return SaveResult{}, err
	}
	if err := validateTelegramDeploymentFieldsRemoved(data); err != nil {
		return SaveResult{}, err
	}
	candidate, err := Parse(data)
	if err != nil {
		return SaveResult{}, err
	}

	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	currentData, err := os.ReadFile(m.path)
	if err != nil {
		return SaveResult{}, fmt.Errorf("read current config: %w", err)
	}
	currentVersion := configVersion(currentData)
	if expected := normalizeVersion(expectedVersion); expected != "" && expected != currentVersion {
		return SaveResult{}, ErrVersionConflict
	}
	restartRequired := hasRestartRequiredChange(currentData, data)
	fileChanged := !bytes.Equal(currentData, data)
	fileMode := configFileMode(m.path)
	if fileChanged {
		if err := writeFileAtomically(m.path, data, fileMode); err != nil {
			return SaveResult{}, err
		}
	}
	version := configVersion(data)
	settings, err := m.publishLocked(candidate, version)
	if err != nil {
		if fileChanged {
			if restoreErr := writeFileAtomically(m.path, currentData, fileMode); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore config after live apply failure: %w", restoreErr))
			}
		}
		return SaveResult{}, fmt.Errorf("apply live configuration: %w", err)
	}
	return SaveResult{
		Version:         version,
		RestartRequired: restartRequired,
		Settings:        settings,
	}, nil
}

// MigrateLegacyRuntimeSettings performs a one-time schema migration into the
// real YAML document. Existing YAML values always win over SQLite values;
// cron_hour is converted to start_time and then removed to avoid two competing
// fields. Missing timezone values are made explicit so future scheduling no
// longer depends on the host. The built-in tag switch is migrated from SQLite
// when the YAML field is absent. Retired settings and the unused drives field
// are removed at the same boundary; comments and unrelated unknown nodes are
// retained by yaml.Node encoding.
func (m *Manager) MigrateLegacyRuntimeSettings(legacy LegacyRuntimeSettings) (bool, error) {
	if m == nil {
		return false, errors.New("configuration manager is unavailable")
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	data, err := os.ReadFile(m.path)
	if err != nil {
		return false, fmt.Errorf("read config for migration: %w", err)
	}
	parsed, err := Parse(data)
	if err != nil {
		return false, err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return false, fmt.Errorf("parse config for migration: %w", err)
	}
	document := ensureDocumentMapping(&root)
	nightly := ensureMappingValue(document, "nightly")
	changed := false

	if _, exists := mappingValue(nightly, "start_time"); !exists {
		startTime := parsed.Nightly.StartTime
		if legacy.NightlyStartTime != nil {
			if normalized, normalizeErr := NormalizeNightlyStartTime(*legacy.NightlyStartTime); normalizeErr == nil {
				startTime = normalized
			}
		}
		setScalarValue(nightly, "start_time", startTime)
		changed = true
	}
	if deleteMappingValue(nightly, "cron_hour") {
		changed = true
	}
	if _, exists := mappingValue(nightly, "timezone"); !exists {
		setScalarValue(nightly, "timezone", parsed.Nightly.Timezone)
		changed = true
	}
	if deleteMappingValue(document, "tags") {
		changed = true
	}

	// Drive definitions have always been persisted and loaded from SQLite. Any
	// YAML drives node is therefore dead configuration and can contain stale
	// credentials. Remove the complete node regardless of whether it is empty.
	if deleteMappingValue(document, "drives") {
		changed = true
	}
	// The former per-drive preview limit and shared media budget no longer
	// control generation. Remove them so the source editor cannot present
	// ineffective controls alongside the three independent global limits.
	preview, _ := mappingValue(document, "preview")
	if deleteMappingValue(preview, "concurrency") {
		changed = true
	}
	generation, _ := mappingValue(document, "generation")
	if deleteMappingValue(generation, "media_concurrency") {
		changed = true
	}

	if !changed {
		if _, err := m.publishLocked(parsed, configVersion(data)); err != nil {
			return false, err
		}
		return false, nil
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		_ = encoder.Close()
		return false, fmt.Errorf("encode migrated config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return false, fmt.Errorf("encode migrated config: %w", err)
	}
	migratedData := out.Bytes()
	migrated, err := Parse(migratedData)
	if err != nil {
		return false, fmt.Errorf("validate migrated config: %w", err)
	}
	fileMode := configFileMode(m.path)
	if err := writeFileAtomically(m.path, migratedData, fileMode); err != nil {
		return false, err
	}
	if _, err := m.publishLocked(migrated, configVersion(migratedData)); err != nil {
		if restoreErr := writeFileAtomically(m.path, data, fileMode); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore config after migration apply failure: %w", restoreErr))
		}
		return false, fmt.Errorf("apply migrated live configuration: %w", err)
	}
	return true, nil
}

// Reload applies an externally edited valid file. Invalid intermediate files
// are ignored, keeping the last known-good runtime snapshot in place.
func (m *Manager) Reload() (bool, error) {
	if m == nil {
		return false, errors.New("configuration manager is unavailable")
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	data, err := os.ReadFile(m.path)
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}
	version := configVersion(data)
	m.mu.RLock()
	observedVersion := m.observedVersion
	m.mu.RUnlock()
	if version == observedVersion {
		return false, nil
	}
	if err := validateAdminConfigRemoved(data); err != nil {
		m.mu.Lock()
		m.observedVersion = version
		m.mu.Unlock()
		return false, err
	}
	parsed, err := Parse(data)
	if err != nil {
		m.mu.Lock()
		m.observedVersion = version
		m.mu.Unlock()
		return false, err
	}
	if _, err := m.publishLocked(parsed, version); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) Watch(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(configWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := m.Reload()
			if err != nil {
				log.Printf("[config] external config reload rejected: %v", err)
			} else if changed {
				log.Printf("[config] external config change loaded; live fields applied")
			}
		}
	}
}

// publishLocked requires updateMu. Changed live settings are applied before the
// validated snapshot becomes visible. A failed callback is asked to restore the
// previous projection, while callers retain or restore the previous YAML bytes.
func (m *Manager) publishLocked(cfg *Config, version string) (LiveSettings, error) {
	settings := liveSettingsFromConfig(cfg)
	m.mu.RLock()
	previous := liveSettingsFromConfig(m.current)
	apply := m.apply
	m.mu.RUnlock()

	if apply != nil && settings != previous {
		if err := apply(settings); err != nil {
			if rollbackErr := apply(previous); rollbackErr != nil {
				return settings, errors.Join(err, fmt.Errorf("restore previous live settings: %w", rollbackErr))
			}
			return settings, err
		}
	}

	m.mu.Lock()
	m.current = cfg
	m.observedVersion = version
	m.mu.Unlock()
	return settings, nil
}

func configVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizeVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "W/")
	return strings.Trim(value, `"`)
}

func mappingValue(parent *yaml.Node, key string) (*yaml.Node, bool) {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i+1], true
		}
	}
	return nil, false
}

func deleteMappingValue(parent *yaml.Node, key string) bool {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content = append(parent.Content[:i], parent.Content[i+2:]...)
			return true
		}
	}
	return false
}

func hasRestartRequiredChange(before, after []byte) bool {
	var beforeDocument any
	var afterDocument any
	if yaml.Unmarshal(before, &beforeDocument) != nil || yaml.Unmarshal(after, &afterDocument) != nil {
		return true
	}
	removeLiveDocumentValues(beforeDocument)
	removeLiveDocumentValues(afterDocument)
	return !reflect.DeepEqual(beforeDocument, afterDocument)
}

func removeLiveDocumentValues(document any) {
	root, ok := document.(map[string]any)
	if !ok {
		return
	}
	delete(root, "telegram")
	removeNestedValue(root, "nightly", "start_time")
	removeNestedValue(root, "nightly", "cron_hour")
	removeNestedValue(root, "nightly", "timezone")
	removeNestedValue(root, "nightly", "disabled")
	delete(root, "tags")
	removeNestedValue(root, "preview", "enabled")
	removeNestedValue(root, "generation", "preview_concurrency")
	removeNestedValue(root, "generation", "thumbnail_concurrency")
	removeNestedValue(root, "generation", "fingerprint_concurrency")
}

func removeNestedValue(root map[string]any, section, key string) {
	value, ok := root[section]
	if !ok {
		return
	}
	mapping, ok := value.(map[string]any)
	if !ok {
		return
	}
	delete(mapping, key)
	if len(mapping) == 0 {
		delete(root, section)
	}
}

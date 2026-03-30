package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func (a *app) deleteDownloadedModel(name string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	modelPath := ""
	downloaded, err := a.discoverDownloadedModels()
	if err != nil {
		return false, err
	}
	for _, model := range downloaded {
		if model.Name == name {
			modelPath = model.BasePath
			break
		}
	}
	if strings.TrimSpace(modelPath) == "" {
		return false, errDownloadedModelNotFound
	}

	cleanRepoPath := filepath.Clean(a.repoPath)
	cleanModelPath := filepath.Clean(modelPath)
	relPath, relErr := filepath.Rel(cleanRepoPath, cleanModelPath)
	if relErr != nil || relPath == "." || strings.HasPrefix(relPath, "..") {
		return false, errors.New("resolved model path is outside model repository")
	}

	cfg, err := a.readConfigFile()
	if err != nil {
		return false, err
	}

	filtered := make([]modelConfigEntry, 0, len(cfg.ModelConfigList))
	removedConfigEntries := 0
	for _, entry := range cfg.ModelConfigList {
		if configName(entry) == name {
			removedConfigEntries++
			continue
		}
		filtered = append(filtered, entry)
	}

	filteredMP := make([]mediapipeConfigEntry, 0, len(cfg.MediapipeConfigList))
	for _, entry := range cfg.MediapipeConfigList {
		if entry.Name == name {
			removedConfigEntries++
			continue
		}
		filteredMP = append(filteredMP, entry)
	}

	if removedConfigEntries > 0 {
		cfg.ModelConfigList = filtered
		cfg.MediapipeConfigList = filteredMP
		if err := a.writeConfigFile(cfg); err != nil {
			return false, err
		}
	}

	if err := os.RemoveAll(cleanModelPath); err != nil {
		return false, err
	}

	return removedConfigEntries > 0, nil
}

func (a *app) registerModelConfig(req registerRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	downloaded, err := a.discoverDownloadedModels()
	if err != nil {
		return err
	}

	var found *modelItem
	for idx := range downloaded {
		if downloaded[idx].Name == req.Name {
			found = &downloaded[idx]
			break
		}
	}

	if found == nil {
		return errDownloadedModelNotFound
	}
	if !found.Compatible {
		reason := found.Reason
		if reason == "" {
			reason = errModelIncompatible.Error()
		}
		return fmt.Errorf("%w: %s", errModelIncompatible, reason)
	}

	cfg, err := a.readConfigFile()
	if err != nil {
		return err
	}

	for _, entry := range cfg.ModelConfigList {
		if configName(entry) == req.Name {
			return errModelAlreadyRegistered
		}
	}
	for _, entry := range cfg.MediapipeConfigList {
		if entry.Name == req.Name {
			return errModelAlreadyRegistered
		}
	}

	if isMediapipeLLMModel(found.BasePath) {
		// For GGUF models: patch graph.pbtxt models_path from "./" to the specific
		// gguf filename so OVMS can resolve the path correctly on Windows.
		if primaryGGUF := findPrimaryGGUFFile(found.BasePath); primaryGGUF != "" {
			if err := patchGraphPbtxtModelPath(found.BasePath, primaryGGUF); err != nil {
				log.Printf("warning: could not patch graph.pbtxt models_path for %s: %v", found.BasePath, err)
			}
		}
		cfg.MediapipeConfigList = append(cfg.MediapipeConfigList, mediapipeConfigEntry{
			Name:     req.Name,
			BasePath: filepath.ToSlash(found.BasePath),
		})
		return a.writeConfigFile(cfg)
	}

	servingBasePath, err := prepareModelServingBasePath(found.BasePath)
	if err != nil {
		return fmt.Errorf("failed to prepare model for OVMS serving: %w", err)
	}

	cfg.ModelConfigList = append(cfg.ModelConfigList, modelConfigEntry{Config: map[string]any{
		"name":          req.Name,
		"base_path":     filepath.ToSlash(servingBasePath),
		"target_device": req.TargetDevice,
	}})

	return a.writeConfigFile(cfg)
}

func (a *app) unregisterModelConfig(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	cfg, err := a.readConfigFile()
	if err != nil {
		return err
	}

	filtered := make([]modelConfigEntry, 0, len(cfg.ModelConfigList))
	removed := false
	for _, entry := range cfg.ModelConfigList {
		if configName(entry) == name {
			removed = true
			continue
		}
		filtered = append(filtered, entry)
	}

	filteredMP := make([]mediapipeConfigEntry, 0, len(cfg.MediapipeConfigList))
	for _, entry := range cfg.MediapipeConfigList {
		if entry.Name == name {
			removed = true
			continue
		}
		filteredMP = append(filteredMP, entry)
	}

	if !removed {
		return errModelNotRegistered
	}

	cfg.ModelConfigList = filtered
	cfg.MediapipeConfigList = filteredMP
	return a.writeConfigFile(cfg)
}

func (a *app) prepareRegisteredModelsForServing() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	cfg, err := a.readConfigFile()
	if err != nil {
		return err
	}

	changed := false
	for idx, entry := range cfg.ModelConfigList {
		basePath := strings.TrimSpace(configBasePath(entry))
		if basePath == "" {
			continue
		}

		preparedBasePath, err := prepareModelServingBasePath(filepath.FromSlash(basePath))
		if err != nil {
			name := configName(entry)
			if name == "" {
				name = basePath
			}
			return fmt.Errorf("%s: %w", name, err)
		}

		if filepath.Clean(preparedBasePath) != filepath.Clean(filepath.FromSlash(basePath)) {
			cfg.ModelConfigList[idx].Config["base_path"] = filepath.ToSlash(preparedBasePath)
			changed = true
		}
	}
	// Mediapipe LLM pipeline models: base_path stays as model root, no serving layout needed.

	if changed {
		if err := a.writeConfigFile(cfg); err != nil {
			return err
		}
	}

	return nil
}

func prepareModelServingBasePath(modelPath string) (string, error) {
	cleanPath := filepath.Clean(modelPath)

	hasVersion, err := directoryHasNumericVersionSubdirs(cleanPath)
	if err != nil {
		return "", err
	}

	if hasVersion {
		return cleanPath, nil
	}

	needsServingLayout, err := needsServingLayoutWithoutVersion(cleanPath)
	if err != nil {
		return "", err
	}

	if needsServingLayout {
		servingBasePath := filepath.Join(cleanPath, ovmsServingArtifactsDirName)
		if err := ensureServingVersionLayout(cleanPath, servingBasePath); err != nil {
			return "", err
		}
		return servingBasePath, nil
	}

	return cleanPath, nil
}

func needsServingLayoutWithoutVersion(modelPath string) (bool, error) {
	if hasOVMSGenAILayout(modelPath) {
		return true, nil
	}

	hasSupportedFiles, err := directoryHasSupportedModelFiles(modelPath)
	if err != nil {
		return false, err
	}

	return hasSupportedFiles, nil
}

func ensureServingVersionLayout(sourceModelPath string, servingBasePath string) error {
	cleanSourcePath := filepath.Clean(sourceModelPath)
	cleanServingBasePath := filepath.Clean(servingBasePath)

	if filepath.Clean(cleanSourcePath) == filepath.Clean(cleanServingBasePath) {
		return errors.New("serving base path must be different from model source path")
	}

	hasVersion, err := directoryHasNumericVersionSubdirs(cleanServingBasePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if hasVersion {
		return nil
	}

	versionPath := filepath.Join(cleanServingBasePath, "1")
	legacyZeroVersionPath := filepath.Join(cleanServingBasePath, "0")

	if _, statErr := os.Stat(versionPath); errors.Is(statErr, os.ErrNotExist) {
		if legacyInfo, legacyErr := os.Stat(legacyZeroVersionPath); legacyErr == nil && legacyInfo.IsDir() {
			if renameErr := os.Rename(legacyZeroVersionPath, versionPath); renameErr == nil {
				return nil
			}
		}
	}

	if err := os.MkdirAll(versionPath, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(cleanSourcePath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := strings.TrimSpace(entry.Name())
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}

		lowerName := strings.ToLower(name)
		if strings.HasPrefix(lowerName, "graph.pbtxt") {
			continue
		}

		src := filepath.Join(cleanSourcePath, name)
		dst := filepath.Join(versionPath, name)
		if _, statErr := os.Stat(dst); statErr == nil {
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}

		if err := linkOrCopyFile(src, dst); err != nil {
			return err
		}
	}

	return nil
}

func linkOrCopyFile(src string, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return out.Sync()
}

func (a *app) ensureModelRepository() error {
	if err := os.MkdirAll(a.repoPath, 0o755); err != nil {
		return err
	}
	return nil
}

func (a *app) ensureConfigFile() error {
	if _, err := os.Stat(a.configPath); err == nil {
		_, err := a.readConfigFile()
		if err == nil {
			return nil
		}
		return fmt.Errorf("config exists but is invalid: %w", err)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	emptyConfig := serverConfig{ModelConfigList: []modelConfigEntry{}}
	return a.writeConfigFile(emptyConfig)
}

func (a *app) readConfigFile() (serverConfig, error) {
	bytes, err := os.ReadFile(a.configPath)
	if err != nil {
		return serverConfig{}, err
	}

	var cfg serverConfig
	if len(bytes) == 0 {
		cfg.ModelConfigList = []modelConfigEntry{}
		return cfg, nil
	}

	// Strip BOM if present
	if len(bytes) >= 3 && bytes[0] == 0xEF && bytes[1] == 0xBB && bytes[2] == 0xBF {
		bytes = bytes[3:]
	}

	if err := json.Unmarshal(bytes, &cfg); err != nil {
		return serverConfig{}, fmt.Errorf("invalid config JSON: %w", err)
	}
	if cfg.ModelConfigList == nil {
		cfg.ModelConfigList = []modelConfigEntry{}
	}
	if cfg.MediapipeConfigList == nil {
		cfg.MediapipeConfigList = []mediapipeConfigEntry{}
	}
	return cfg, nil
}

func (a *app) writeConfigFile(cfg serverConfig) error {
	if cfg.ModelConfigList == nil {
		cfg.ModelConfigList = []modelConfigEntry{}
	}
	// Omit mediapipe_config_list from JSON when empty to keep config minimal.
	if len(cfg.MediapipeConfigList) == 0 {
		cfg.MediapipeConfigList = nil
	}

	bytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(a.configPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(a.configPath, bytes, 0o644)
}

func configName(entry modelConfigEntry) string {
	if entry.Config == nil {
		return ""
	}
	value, ok := entry.Config["name"]
	if !ok {
		return ""
	}
	name, _ := value.(string)
	return strings.TrimSpace(name)
}

func configBasePath(entry modelConfigEntry) string {
	if entry.Config == nil {
		return ""
	}
	value, ok := entry.Config["base_path"]
	if !ok {
		return ""
	}
	basePath, _ := value.(string)
	return basePath
}

func configTargetDevice(entry modelConfigEntry) string {
	if entry.Config == nil {
		return ""
	}
	value, ok := entry.Config["target_device"]
	if !ok {
		return ""
	}
	target, _ := value.(string)
	return target
}

func deriveModelName(sourceModel string) string {
	trimmed := strings.TrimSpace(sourceModel)
	if trimmed == "" {
		return "model"
	}

	parts := strings.Split(trimmed, "/")
	name := strings.TrimSpace(parts[len(parts)-1])
	name = modelNameSanitizer.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-._")
	if name == "" {
		return "model"
	}
	return name
}

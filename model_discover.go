package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func (a *app) listModels() (modelListResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	downloaded, err := a.discoverDownloadedModels()
	if err != nil {
		return modelListResponse{}, err
	}

	cfg, err := a.readConfigFile()
	if err != nil {
		return modelListResponse{}, err
	}

	downloadedByName := make(map[string]*modelItem, len(downloaded))
	for idx := range downloaded {
		downloadedByName[downloaded[idx].Name] = &downloaded[idx]
	}

	registered := make([]modelItem, 0, len(cfg.ModelConfigList)+len(cfg.MediapipeConfigList))
	for _, entry := range cfg.ModelConfigList {
		name := configName(entry)
		if name == "" {
			continue
		}

		basePath := configBasePath(entry)
		targetDevice := configTargetDevice(entry)
		item := modelItem{
			Name:         name,
			BasePath:     basePath,
			Registered:   true,
			TargetDevice: targetDevice,
		}

		if downloadedModel, ok := downloadedByName[name]; ok {
			downloadedModel.Registered = true
			downloadedModel.TargetDevice = targetDevice
			item.Compatible = downloadedModel.Compatible
			item.Reason = downloadedModel.Reason
		} else {
			item.Compatible = false
			item.Reason = "model path not found in repository"
		}

		registered = append(registered, item)
	}
	for _, entry := range cfg.MediapipeConfigList {
		if entry.Name == "" {
			continue
		}
		item := modelItem{
			Name:       entry.Name,
			BasePath:   entry.BasePath,
			Registered: true,
		}
		if downloadedModel, ok := downloadedByName[entry.Name]; ok {
			downloadedModel.Registered = true
			item.Compatible = downloadedModel.Compatible
			item.Reason = downloadedModel.Reason
		} else {
			item.Compatible = false
			item.Reason = "model path not found in repository"
		}
		registered = append(registered, item)
	}

	sort.Slice(downloaded, func(i, j int) bool {
		return downloaded[i].Name < downloaded[j].Name
	})
	sort.Slice(registered, func(i, j int) bool {
		return registered[i].Name < registered[j].Name
	})

	return modelListResponse{
		DownloadedModels: downloaded,
		RegisteredModels: registered,
	}, nil
}

func (a *app) discoverDownloadedModels() ([]modelItem, error) {
	modelPaths, err := discoverModelDirectories(a.repoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to discover model directories: %w", err)
	}

	nameCounts := make(map[string]int)
	for _, modelPath := range modelPaths {
		nameCounts[strings.ToLower(filepath.Base(modelPath))]++
	}

	models := make([]modelItem, 0, len(modelPaths))
	for _, modelPath := range modelPaths {
		relPath, relErr := filepath.Rel(a.repoPath, modelPath)
		if relErr != nil || strings.HasPrefix(relPath, "..") {
			relPath = filepath.Base(modelPath)
		}
		relPath = filepath.ToSlash(relPath)

		displayName := filepath.Base(modelPath)
		if nameCounts[strings.ToLower(displayName)] > 1 {
			displayName = relPath
		}

		compatible, reason := detectOVMSCompatibility(modelPath)
		models = append(models, modelItem{
			Name:       displayName,
			BasePath:   modelPath,
			Compatible: compatible,
			Reason:     reason,
		})
	}

	return models, nil
}

func discoverModelDirectories(repoPath string) ([]string, error) {
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	modelPaths := make([]string, 0)
	addPath := func(path string) {
		clean := filepath.Clean(path)
		if _, exists := seen[clean]; exists {
			return
		}
		seen[clean] = struct{}{}
		modelPaths = append(modelPaths, clean)
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.EqualFold(entry.Name(), ovmsServingArtifactsDirName) {
			continue
		}

		childPath := filepath.Join(repoPath, entry.Name())
		hasDirectVersion, err := directoryHasNumericVersionSubdirs(childPath)
		if err == nil && hasDirectVersion {
			addPath(childPath)
			continue
		}

		childModelDirs, err := listImmediateModelDirs(childPath)
		if err == nil && len(childModelDirs) > 0 {
			for _, modelDir := range childModelDirs {
				addPath(modelDir)
			}
			continue
		}

		hasModelFiles, err := directoryHasSupportedModelFiles(childPath)
		if err == nil && hasModelFiles {
			addPath(childPath)
			continue
		}

		nestedVersionDirs, err := findNestedModelDirsByVersion(childPath)
		if err == nil && len(nestedVersionDirs) > 0 {
			for _, modelDir := range nestedVersionDirs {
				addPath(modelDir)
			}
			continue
		}

		addPath(childPath)
	}

	sort.Strings(modelPaths)

	return modelPaths, nil
}

func findNestedModelDirsByVersion(rootPath string) ([]string, error) {
	found := make([]string, 0)
	seen := make(map[string]struct{})

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == rootPath {
			return nil
		}
		if !d.IsDir() {
			return nil
		}

		if strings.HasPrefix(d.Name(), ".") || strings.EqualFold(d.Name(), ovmsServingArtifactsDirName) {
			return filepath.SkipDir
		}

		hasVersion, err := directoryHasNumericVersionSubdirs(path)
		if err != nil {
			return nil
		}
		if hasVersion {
			clean := filepath.Clean(path)
			if _, exists := seen[clean]; !exists {
				seen[clean] = struct{}{}
				found = append(found, clean)
			}
			return filepath.SkipDir
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return found, nil
}

func listImmediateModelDirs(parentPath string) ([]string, error) {
	entries, err := os.ReadDir(parentPath)
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.EqualFold(entry.Name(), ovmsServingArtifactsDirName) {
			continue
		}

		candidate := filepath.Join(parentPath, entry.Name())
		hasVersion, err := directoryHasNumericVersionSubdirs(candidate)
		if err == nil && hasVersion {
			paths = append(paths, candidate)
			continue
		}

		hasFiles, err := directoryHasSupportedModelFiles(candidate)
		if err == nil && hasFiles {
			paths = append(paths, candidate)
			continue
		}

		if looksLikeModelDirName(entry.Name()) {
			paths = append(paths, candidate)
		}
	}

	return paths, nil
}

func directoryHasSupportedModelFiles(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}

	hasXML := false
	hasBIN := false
	hasONNX := false
	hasPDModel := false
	hasPDParams := false
	hasSavedModel := false
	hasTFLite := false
	hasGGUF := false

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		ext := strings.ToLower(filepath.Ext(name))

		switch {
		case ext == ".xml":
			hasXML = true
		case ext == ".bin":
			hasBIN = true
		case ext == ".onnx":
			hasONNX = true
		case ext == ".pdmodel":
			hasPDModel = true
		case ext == ".pdiparams":
			hasPDParams = true
		case name == "saved_model.pb":
			hasSavedModel = true
		case ext == ".tflite":
			hasTFLite = true
		case ext == ".gguf":
			hasGGUF = true
		}
	}

	return (hasXML && hasBIN) || hasONNX || (hasPDModel && hasPDParams) || hasSavedModel || hasTFLite || hasGGUF, nil
}

func looksLikeModelDirName(name string) bool {
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if lowerName == "" {
		return false
	}
	if lowerName == "openvino" {
		return false
	}
	return strings.Contains(lowerName, "-ov") || strings.Contains(lowerName, "_ov")
}

func directoryHasNumericVersionSubdirs(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		versionNumber, parseErr := strconv.Atoi(entry.Name())
		if parseErr == nil && versionNumber > 0 {
			return true, nil
		}
	}

	return false, nil
}

func detectOVMSCompatibility(modelPath string) (bool, string) {
	versionDirs, err := listVersionDirectories(modelPath)
	if err != nil {
		return false, "cannot inspect model directory"
	}
	if len(versionDirs) == 0 {
		needsServingLayout, layoutErr := needsServingLayoutWithoutVersion(modelPath)
		if layoutErr == nil && needsServingLayout {
			return true, ""
		}
		return false, "no numeric version folder found"
	}

	for _, versionPath := range versionDirs {
		if versionContainsSupportedFormat(versionPath) {
			return true, ""
		}
	}

	if hasOVMSGenAILayout(modelPath) {
		return true, ""
	}

	return false, "missing supported model files in version folder"
}

// isMediapipeLLMModel returns true when the model folder contains a graph.pbtxt
// that references an LLM calculator. These models must be registered under
// mediapipe_config_list (not model_config_list) so OVMS loads them as a
// Mediapipe pipeline graph rather than a classic versioned model.
func isMediapipeLLMModel(modelPath string) bool {
	graphPath := filepath.Join(modelPath, "graph.pbtxt")
	data, err := os.ReadFile(graphPath)
	if err != nil {
		return false
	}
	content := string(data)
	return strings.Contains(content, "HttpLLMCalculator") || strings.Contains(content, "LLMCalculator")
}

// findPrimaryGGUFFile returns the filename (not full path) of the main GGUF model
// file in modelPath, skipping multimodal projector / embedding files.
func findPrimaryGGUFFile(modelPath string) string {
	skipPatterns := []string{"mmproj", "mmpt", "-embed", "_embed", "vision_encoder", "-proj"}
	entries, err := os.ReadDir(modelPath)
	if err != nil {
		return ""
	}
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.EqualFold(filepath.Ext(name), ".gguf") {
			continue
		}
		lower := strings.ToLower(name)
		skip := false
		for _, pat := range skipPatterns {
			if strings.Contains(lower, pat) {
				skip = true
				break
			}
		}
		if !skip {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	// Prefer quantization types that OVMS's GGUF parser supports reliably.
	// K-quants (Q4_K_M, Q6_K, etc.) can cause gguf_tensor_to_f16 failures.
	preferOrder := []string{"q8_0", "q4_0", "q5_0", "q5_1", "q4_1", "f16", "f32"}
	for _, pref := range preferOrder {
		for _, c := range candidates {
			if strings.Contains(strings.ToLower(c), pref) {
				return c
			}
		}
	}
	// Fallback: alphabetical
	sort.Strings(candidates)
	return candidates[0]
}

// patchGraphPbtxtModelPath rewrites the models_path field in graph.pbtxt from
// "./" to ggufFilename so OVMS can resolve the path correctly on Windows.
func patchGraphPbtxtModelPath(modelPath string, ggufFilename string) error {
	graphPath := filepath.Join(modelPath, "graph.pbtxt")
	data, err := os.ReadFile(graphPath)
	if err != nil {
		return err
	}
	content := string(data)
	updated := ggufModelsDirRegex.ReplaceAllString(content, `$1"`+ggufFilename+`",`)
	if updated == content {
		return nil // pattern not found or already updated
	}
	return os.WriteFile(graphPath, []byte(updated), 0o644)
}

func hasOVMSGenAILayout(modelPath string) bool {
	entries, err := os.ReadDir(modelPath)
	if err != nil {
		return false
	}

	hasOpenVINOConfig := false
	hasGraphConfig := false
	hasModelIndex := false
	hasOpenVINOModelFile := false
	hasComponentModel := false
	hasGGUF := false

	for _, entry := range entries {
		name := strings.ToLower(entry.Name())

		if entry.IsDir() {
			componentPath := filepath.Join(modelPath, entry.Name())
			componentHasModel, componentErr := directoryHasSupportedModelFiles(componentPath)
			if componentErr == nil && componentHasModel {
				hasComponentModel = true
			}
			continue
		}

		switch name {
		case "openvino_config.json":
			hasOpenVINOConfig = true
		case "graph.pbtxt":
			hasGraphConfig = true
		case "model_index.json":
			hasModelIndex = true
		}

		if strings.HasPrefix(name, "openvino_model.") {
			hasOpenVINOModelFile = true
		}
		if strings.ToLower(filepath.Ext(name)) == ".gguf" {
			hasGGUF = true
		}
	}

	if hasOpenVINOConfig && (hasOpenVINOModelFile || hasGraphConfig || hasModelIndex || hasComponentModel) {
		return true
	}

	// graph.pbtxt + GGUF file at root = valid GenAI GGUF pipeline layout
	if hasGraphConfig && hasGGUF {
		return true
	}

	if hasGraphConfig && (hasModelIndex || hasComponentModel) {
		return true
	}

	return false
}

func listVersionDirectories(modelPath string) ([]string, error) {
	entries, err := os.ReadDir(modelPath)
	if err != nil {
		return nil, err
	}

	versionDirs := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		versionNumber, err := strconv.Atoi(entry.Name())
		if err != nil || versionNumber <= 0 {
			continue
		}
		versionDirs = append(versionDirs, filepath.Join(modelPath, entry.Name()))
	}

	return versionDirs, nil
}

func versionContainsSupportedFormat(versionPath string) bool {
	entries, err := os.ReadDir(versionPath)
	if err != nil {
		return false
	}

	hasXML := false
	hasBIN := false
	hasONNX := false
	hasPDModel := false
	hasPDParams := false
	hasSavedModel := false
	hasTFLite := false
	hasGGUF := false

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		ext := strings.ToLower(filepath.Ext(name))

		switch {
		case ext == ".xml":
			hasXML = true
		case ext == ".bin":
			hasBIN = true
		case ext == ".onnx":
			hasONNX = true
		case ext == ".pdmodel":
			hasPDModel = true
		case ext == ".pdiparams":
			hasPDParams = true
		case name == "saved_model.pb":
			hasSavedModel = true
		case ext == ".tflite":
			hasTFLite = true
		case ext == ".gguf":
			hasGGUF = true
		}
	}

	return (hasXML && hasBIN) || hasONNX || (hasPDModel && hasPDParams) || hasSavedModel || hasTFLite || hasGGUF
}

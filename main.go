package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/*
var webFS embed.FS

var (
	modelNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
	serviceStateRegex  = regexp.MustCompile(`(?m)STATE\s*:\s*\d+\s+([A-Z_]+)`)
	versionRegex       = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:[-+._][0-9A-Za-z]+)?`)
	versionTriplet     = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)
	downloadProgressRe = regexp.MustCompile(`(\d{1,3})\s*%`)
)

const ovmsServiceName = "ovms"
const ovmsServingArtifactsDirName = "_ovms_serving"

const ovmsOpenAIModelsURL = "http://127.0.0.1:8000/v3/models"

var (
	errDownloadedModelNotFound = errors.New("downloaded model not found")
	errModelAlreadyRegistered  = errors.New("model already registered")
	errModelNotRegistered      = errors.New("model is not registered")
	errModelIncompatible       = errors.New("model is not compatible with OVMS")
)

type app struct {
	repoPath     string
	configPath   string
	ovmsExePath  string
	catalogLimit int
	jobCounter   int64
	downloadJobs map[string]*downloadJob
	mu           sync.Mutex
}

type serverConfig struct {
	ModelConfigList     []modelConfigEntry     `json:"model_config_list"`
	MediapipeConfigList []mediapipeConfigEntry `json:"mediapipe_config_list,omitempty"`
}

type modelConfigEntry struct {
	Config map[string]any `json:"config"`
}

type mediapipeConfigEntry struct {
	Name     string `json:"name"`
	BasePath string `json:"base_path"`
}

type modelItem struct {
	Name         string `json:"name"`
	BasePath     string `json:"basePath"`
	Compatible   bool   `json:"compatible"`
	Reason       string `json:"reason,omitempty"`
	Registered   bool   `json:"registered"`
	TargetDevice string `json:"targetDevice,omitempty"`
}

type modelListResponse struct {
	DownloadedModels []modelItem `json:"downloadedModels"`
	RegisteredModels []modelItem `json:"registeredModels"`
}

type registerRequest struct {
	Name         string `json:"name"`
	TargetDevice string `json:"targetDevice"`
}

type unregisterRequest struct {
	Name string `json:"name"`
}

type deleteModelRequest struct {
	Name string `json:"name"`
}

type catalogItem struct {
	ID            string   `json:"id"`
	Downloads     int      `json:"downloads"`
	Likes         int      `json:"likes"`
	Task          string   `json:"task,omitempty"`
	SuggestedTask string   `json:"suggestedTask,omitempty"`
	LastModified  string   `json:"lastModified,omitempty"`
	Tags          []string `json:"tags,omitempty"`
}

type catalogResponse struct {
	Items       []catalogItem `json:"items"`
	Page        int           `json:"page"`
	PageSize    int           `json:"pageSize"`
	Total       int           `json:"total"`
	TotalPages  int           `json:"totalPages"`
	HasMore     bool          `json:"hasMore"`
	FetchOffset int           `json:"fetchOffset"`
}

type downloadRequest struct {
	SourceModel  string `json:"sourceModel"`
	ModelName    string `json:"modelName"`
	TargetDevice string `json:"targetDevice"`
	Task         string `json:"task"`
	Overwrite    bool   `json:"overwrite"`
}

type hfCatalogModel struct {
	ID           string   `json:"id"`
	Downloads    int      `json:"downloads"`
	Likes        int      `json:"likes"`
	PipelineTag  string   `json:"pipeline_tag"`
	LastModified string   `json:"lastModified"`
	Tags         []string `json:"tags"`
}

type downloadJob struct {
	ID           string `json:"id"`
	SourceModel  string `json:"sourceModel"`
	ModelName    string `json:"modelName"`
	TargetDevice string `json:"targetDevice"`
	Task         string `json:"task,omitempty"`
	Status       string `json:"status"`
	Progress     int    `json:"progress"`
	ProgressText string `json:"progressText,omitempty"`
	Error        string `json:"error,omitempty"`
	Output       string `json:"output,omitempty"`
	CreatedAt    string `json:"createdAt"`
	StartedAt    string `json:"startedAt,omitempty"`
	FinishedAt   string `json:"finishedAt,omitempty"`
}

type downloadAcceptedResponse struct {
	Job downloadJob `json:"job"`
}

type downloadJobListResponse struct {
	Jobs []downloadJob `json:"jobs"`
}

type ovmsStatusResponse struct {
	ServiceName           string `json:"serviceName"`
	ServiceInstalled      bool   `json:"serviceInstalled"`
	ServiceRunning        bool   `json:"serviceRunning"`
	ServiceState          string `json:"serviceState"`
	ServiceError          string `json:"serviceError,omitempty"`
	InstalledVersion      string `json:"installedVersion,omitempty"`
	InstalledVersionError string `json:"installedVersionError,omitempty"`
	LatestVersion         string `json:"latestVersion,omitempty"`
	LatestVersionURL      string `json:"latestVersionUrl,omitempty"`
	VersionCheckError     string `json:"versionCheckError,omitempty"`
	UpdateAvailable       bool   `json:"updateAvailable"`
}

type ovmsActionResponse struct {
	Success bool               `json:"success"`
	Message string             `json:"message"`
	Output  string             `json:"output,omitempty"`
	Status  ovmsStatusResponse `json:"status"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
}

func main() {
	repoPath := flag.String("repo", `.\models_repo`, "Model repository path")
	configPath := flag.String("config", "", "Path to OVMS config.json (default: <repo>/config.json)")
	ovmsExePath := flag.String("ovmsExe", `ovms\ovms.exe`, "Path to ovms executable used for model download action")
	catalogLimit := flag.Int("catalogLimit", 200, "Maximum catalog models fetched from internet source before pagination")
	port := flag.Int("port", 8090, "Port for web UI and API")
	flag.Parse()

	resolvedRepoPath, err := filepath.Abs(strings.TrimSpace(*repoPath))
	if err != nil {
		log.Fatalf("invalid repo path: %v", err)
	}

	resolvedConfigPath := *configPath
	if strings.TrimSpace(resolvedConfigPath) == "" {
		resolvedConfigPath = filepath.Join(resolvedRepoPath, "config.json")
	}
	resolvedConfigPath, err = filepath.Abs(strings.TrimSpace(resolvedConfigPath))
	if err != nil {
		log.Fatalf("invalid config path: %v", err)
	}

	instance := &app{
		repoPath:     resolvedRepoPath,
		configPath:   resolvedConfigPath,
		ovmsExePath:  *ovmsExePath,
		catalogLimit: *catalogLimit,
		downloadJobs: map[string]*downloadJob{},
	}

	if err := instance.ensureModelRepository(); err != nil {
		log.Fatalf("failed to prepare model repository: %v", err)
	}
	if err := instance.ensureConfigFile(); err != nil {
		log.Fatalf("failed to prepare config file: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/models", instance.handleModels)
	mux.HandleFunc("/api/catalog", instance.handleCatalog)
	mux.HandleFunc("/api/download", instance.handleDownload)
	mux.HandleFunc("/api/downloads", instance.handleDownloadJobs)
	mux.HandleFunc("/api/register", instance.handleRegister)
	mux.HandleFunc("/api/unregister", instance.handleUnregister)
	mux.HandleFunc("/api/models/delete", instance.handleDeleteModel)
	mux.HandleFunc("/api/reload", instance.handleReload)
	mux.HandleFunc("/api/ovms/status", instance.handleOVMSStatus)
	mux.HandleFunc("/api/ovms/install", instance.handleOVMSInstall)
	mux.HandleFunc("/api/ovms/start", instance.handleOVMSStart)
	mux.HandleFunc("/api/ovms/stop", instance.handleOVMSStop)
	mux.HandleFunc("/api/ovms/update", instance.handleOVMSUpdate)

	subWeb, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("failed to load embedded web files: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(subWeb)))

	address := fmt.Sprintf(":%d", *port)
	log.Printf("OVMS Manager started at http://localhost%s", address)
	log.Printf("Model repository: %s", instance.repoPath)
	log.Printf("Config file: %s", instance.configPath)
	log.Printf("OVMS executable: %s", instance.ovmsExePath)
	if err := http.ListenAndServe(address, withCORS(mux)); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	payload, err := a.listModels()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *app) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	page, pageSize := parseCatalogPagination(r)
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		source = "openvino"
	}
	fetchOffset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if fetchOffset < 0 {
		fetchOffset = 0
	}
	payload, err := a.listCatalogFromInternet(query, page, pageSize, source, fetchOffset)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, payload)
}

func (a *app) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req downloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	req.SourceModel = strings.TrimSpace(req.SourceModel)
	req.ModelName = strings.TrimSpace(req.ModelName)
	req.TargetDevice = strings.TrimSpace(req.TargetDevice)
	req.Task = strings.TrimSpace(req.Task)

	if req.SourceModel == "" {
		writeJSONError(w, http.StatusBadRequest, "sourceModel is required")
		return
	}
	if req.ModelName == "" {
		req.ModelName = deriveModelName(req.SourceModel)
	}
	if req.TargetDevice == "" {
		req.TargetDevice = "CPU"
	}
	req.Task = normalizeOVMSTask(req.Task)
	if req.Task == "" {
		req.Task = inferOVMSTaskFromSource(req.SourceModel)
	}

	job := a.enqueueDownload(req)
	writeJSON(w, http.StatusAccepted, downloadAcceptedResponse{Job: job})
}

func (a *app) handleDownloadJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	jobs := a.listDownloadJobs()
	writeJSON(w, http.StatusOK, downloadJobListResponse{Jobs: jobs})
}

func (a *app) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.TargetDevice == "" {
		req.TargetDevice = "CPU"
	}

	err := a.registerModelConfig(req)
	if err != nil {
		switch {
		case errors.Is(err, errDownloadedModelNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errModelAlreadyRegistered):
			writeJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errModelIncompatible):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	shouldWaitForLoad := true
	if runtime.GOOS == "windows" {
		serviceInstalled, serviceRunning, _, statusErr := queryWindowsServiceStatus(ovmsServiceName)
		if statusErr != nil {
			writeJSONError(w, http.StatusBadGateway, "model registered but failed to verify OVMS service state: "+statusErr.Error())
			return
		}
		if !serviceInstalled || !serviceRunning {
			shouldWaitForLoad = false
		}
	}

	if !shouldWaitForLoad {
		writeJSON(w, http.StatusOK, map[string]string{"message": "model registered. OVMS service is stopped; it will load automatically after Start Service."})
		return
	}

	if err := a.waitForOVMSModelLoad(req.Name, 25*time.Second); err != nil {
		writeJSON(w, http.StatusAccepted, map[string]string{"message": "model registered, but OVMS has not loaded it yet. Check OVMS logs for load errors."})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "model registered and loaded"})
}

func (a *app) handleUnregister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req unregisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}

	err := a.unregisterModelConfig(req.Name)
	if err != nil {
		if errors.Is(err, errModelNotRegistered) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "model unregistered"})
}

func (a *app) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req deleteModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}

	unregistered, err := a.deleteDownloadedModel(req.Name)
	if err != nil {
		if errors.Is(err, errDownloadedModelNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if unregistered {
		writeJSON(w, http.StatusOK, map[string]string{"message": "model deleted and unregistered"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "model deleted"})
}

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

func (a *app) waitForOVMSModelLoad(modelName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		loaded, err := isModelLoadedInOVMS(modelName)
		if err != nil {
			lastErr = err
		} else if loaded {
			return nil
		}
		time.Sleep(1 * time.Second)
	}

	if lastErr != nil {
		return fmt.Errorf("registered model was not loaded by OVMS within %s: %w", timeout.String(), lastErr)
	}
	return fmt.Errorf("registered model was not loaded by OVMS within %s", timeout.String())
}

func isModelLoadedInOVMS(modelName string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, ovmsOpenAIModelsURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ovms-manager-win/1.0")

	client := &http.Client{Timeout: 8 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return false, fmt.Errorf("%s returned status %d: %s", ovmsOpenAIModelsURL, res.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return false, err
	}

	rawData, ok := payload["data"]
	if !ok {
		return false, nil
	}
	items, ok := rawData.([]any)
	if !ok {
		return false, nil
	}

	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"id", "name", "model"} {
			value, ok := itemMap[key]
			if !ok {
				continue
			}
			loadedName, _ := value.(string)
			if strings.EqualFold(strings.TrimSpace(loadedName), strings.TrimSpace(modelName)) {
				return true, nil
			}
		}
	}

	return false, nil
}

func (a *app) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	payload, err := a.listModels()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, payload)
}

func (a *app) handleOVMSStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, a.getOVMSStatus())
}

func (a *app) handleOVMSInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	output, err := a.installOVMSService()
	alreadyInstalled := isOVMSServiceAlreadyInstalledOutput(output)
	status := a.getOVMSStatus()
	response := ovmsActionResponse{
		Success: err == nil || alreadyInstalled,
		Status:  status,
		Output:  truncateText(strings.TrimSpace(output), 3000),
	}
	if err != nil {
		if alreadyInstalled {
			response.Message = "OVMS service already exists. Installation step skipped."
			writeJSON(w, http.StatusOK, response)
			return
		}
		response.Message = formatOVMSInstallError(err, output)
		writeJSON(w, http.StatusOK, response)
		return
	}

	if isOVMSServiceAlreadyInstalledOutput(output) {
		response.Message = "OVMS service already exists. Installation step skipped."
		writeJSON(w, http.StatusOK, response)
		return
	}

	response.Message = "OVMS service installation completed"
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleOVMSStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if err := a.prepareRegisteredModelsForServing(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to prepare model layout for OVMS start: "+err.Error())
		return
	}

	output, err := a.controlOVMSService("start")
	status := a.getOVMSStatus()
	response := ovmsActionResponse{
		Success: err == nil,
		Status:  status,
		Output:  truncateText(strings.TrimSpace(output), 3000),
	}
	if err != nil {
		response.Message = formatOVMSServiceActionError("start", err, output)
		writeJSON(w, http.StatusOK, response)
		return
	}

	response.Message = "OVMS service started"
	if status.ServiceRunning {
		response.Message = "OVMS service is running"
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleOVMSStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	output, err := a.controlOVMSService("stop")
	status := a.getOVMSStatus()
	response := ovmsActionResponse{
		Success: err == nil,
		Status:  status,
		Output:  truncateText(strings.TrimSpace(output), 3000),
	}
	if err != nil {
		response.Message = formatOVMSServiceActionError("stop", err, output)
		writeJSON(w, http.StatusOK, response)
		return
	}

	response.Message = "OVMS service stopped"
	if !status.ServiceRunning {
		response.Message = "OVMS service is stopped"
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleOVMSUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	status := a.getOVMSStatus()
	message := "OVMS update check completed"
	if status.VersionCheckError != "" {
		message = "OVMS update check completed with warnings"
	}

	writeJSON(w, http.StatusOK, ovmsActionResponse{
		Success: status.VersionCheckError == "",
		Message: message,
		Status:  status,
	})
}

func (a *app) getOVMSStatus() ovmsStatusResponse {
	status := ovmsStatusResponse{
		ServiceName:  ovmsServiceName,
		ServiceState: "unknown",
	}

	serviceInstalled, serviceRunning, serviceState, serviceErr := queryWindowsServiceStatus(ovmsServiceName)
	status.ServiceInstalled = serviceInstalled
	status.ServiceRunning = serviceRunning
	status.ServiceState = serviceState
	if serviceErr != nil {
		status.ServiceError = serviceErr.Error()
	}

	installedVersion, installedErr := a.getInstalledOVMSVersion()
	if installedVersion != "" {
		status.InstalledVersion = installedVersion
	}
	if installedErr != nil {
		status.InstalledVersionError = installedErr.Error()
	}

	latestVersion, latestVersionURL, latestErr := fetchLatestOVMSVersion()
	if latestVersion != "" {
		status.LatestVersion = latestVersion
	}
	if latestVersionURL != "" {
		status.LatestVersionURL = latestVersionURL
	}
	if latestErr != nil {
		status.VersionCheckError = latestErr.Error()
	}

	if status.InstalledVersion != "" && status.LatestVersion != "" {
		cmp, cmpErr := compareVersionStrings(status.InstalledVersion, status.LatestVersion)
		if cmpErr != nil {
			status.VersionCheckError = appendError(status.VersionCheckError, cmpErr.Error())
		} else {
			status.UpdateAvailable = cmp < 0
		}
	}

	return status
}

func (a *app) installOVMSService() (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("OVMS service installation is only supported on Windows")
	}

	serviceInstalled, _, _, serviceErr := queryWindowsServiceStatus(ovmsServiceName)
	if serviceErr == nil && serviceInstalled {
		return "OVMS service already exists. Installation step skipped.", nil
	}

	ovmsPath, err := a.resolveOVMSExecutablePath()
	if err != nil {
		return "", err
	}

	scriptPath := filepath.Join(filepath.Dir(ovmsPath), "install_ovms_service.bat")
	if _, err := os.Stat(scriptPath); err != nil {
		return "", fmt.Errorf("install script not found at %s", scriptPath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "cmd", "/C", scriptPath, a.repoPath)
	cmd.Dir = filepath.Dir(scriptPath)
	cmd.Env = prepareOVMSEnv(filepath.Dir(ovmsPath), a.repoPath)

	output, runErr := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(output), errors.New("OVMS service installation timed out")
	}
	if runErr != nil {
		return string(output), fmt.Errorf("OVMS service installation failed: %w", runErr)
	}

	return string(output), nil
}

func (a *app) controlOVMSService(action string) (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("OVMS service control is only supported on Windows")
	}

	action = strings.ToLower(strings.TrimSpace(action))
	if action != "start" && action != "stop" {
		return "", errors.New("unsupported service action")
	}

	serviceInstalled, serviceRunning, _, serviceErr := queryWindowsServiceStatus(ovmsServiceName)
	if serviceErr != nil {
		return "", serviceErr
	}
	if !serviceInstalled {
		return "", errors.New("OVMS service is not installed. Install service first.")
	}

	if action == "start" && serviceRunning {
		return "OVMS service is already running.", nil
	}
	if action == "stop" && !serviceRunning {
		return "OVMS service is already stopped.", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sc", action, ovmsServiceName)
	output, runErr := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	lowerOutput := strings.ToLower(outputText)

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return outputText, fmt.Errorf("OVMS service %s timed out", action)
	}

	if runErr != nil {
		if action == "start" && (strings.Contains(lowerOutput, "failed 1056") || strings.Contains(lowerOutput, "already been started")) {
			return outputText, nil
		}
		if action == "stop" && (strings.Contains(lowerOutput, "failed 1062") || strings.Contains(lowerOutput, "has not been started")) {
			return outputText, nil
		}
		return outputText, fmt.Errorf("OVMS service %s failed: %w", action, runErr)
	}

	return outputText, nil
}

func queryWindowsServiceStatus(serviceName string) (bool, bool, string, error) {
	if runtime.GOOS != "windows" {
		return false, false, "unsupported", errors.New("service status check is only supported on Windows")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sc", "query", serviceName)
	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false, false, "unknown", errors.New("service status check timed out")
	}

	if err != nil {
		upperOutput := strings.ToUpper(outputText)
		if strings.Contains(upperOutput, "FAILED 1060") || strings.Contains(upperOutput, "DOES NOT EXIST") {
			return false, false, "not_installed", nil
		}
		if outputText == "" {
			return false, false, "unknown", fmt.Errorf("service status check failed: %w", err)
		}
		return false, false, "unknown", fmt.Errorf("service status check failed: %s", outputText)
	}

	state := "unknown"
	stateMatch := serviceStateRegex.FindStringSubmatch(outputText)
	if len(stateMatch) > 1 {
		state = strings.ToLower(strings.TrimSpace(stateMatch[1]))
	}

	return true, strings.EqualFold(state, "running"), state, nil
}

func (a *app) getInstalledOVMSVersion() (string, error) {
	ovmsPath, err := a.resolveOVMSExecutablePath()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ovmsPath, "--version")
	cmd.Dir = filepath.Dir(ovmsPath)
	cmd.Env = prepareOVMSEnv(filepath.Dir(ovmsPath), a.repoPath)

	output, runErr := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", errors.New("installed OVMS version check timed out")
	}

	if parsed := extractVersion(outputText); parsed != "" {
		return parsed, nil
	}

	if fileVersion, fileErr := queryWindowsFileVersion(ovmsPath); fileErr == nil {
		if parsed := extractVersion(fileVersion); parsed != "" {
			return parsed, nil
		}
		if strings.TrimSpace(fileVersion) != "" {
			return strings.TrimSpace(fileVersion), nil
		}
	}

	if runErr != nil {
		if outputText == "" {
			return "", fmt.Errorf("OVMS version check failed: %w", runErr)
		}
		return "", fmt.Errorf("OVMS version check failed: %s", outputText)
	}

	if outputText != "" {
		return outputText, nil
	}

	return "", errors.New("installed OVMS version is unavailable")
}

func queryWindowsFileVersion(filePath string) (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("file version check is only supported on Windows")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	escapedPath := strings.ReplaceAll(filePath, "'", "''")
	command := fmt.Sprintf("(Get-Item -LiteralPath '%s').VersionInfo.ProductVersion", escapedPath)
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", command)
	output, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", errors.New("file version query timed out")
	}
	if err != nil {
		outputText := strings.TrimSpace(string(output))
		if outputText == "" {
			return "", fmt.Errorf("file version query failed: %w", err)
		}
		return "", fmt.Errorf("file version query failed: %s", outputText)
	}

	return strings.TrimSpace(string(output)), nil
}

func fetchLatestOVMSVersion() (string, string, error) {
	endpoint := "https://api.github.com/repos/openvinotoolkit/model_server/releases/latest"
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}

	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "ovms-manager-win/1.0")

	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("latest OVMS version request failed: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return "", "", fmt.Errorf("latest OVMS version request failed with status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var release githubRelease
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return "", "", fmt.Errorf("latest OVMS version response parse failed: %w", err)
	}

	rawVersion := strings.TrimSpace(release.TagName)
	if rawVersion == "" {
		rawVersion = strings.TrimSpace(release.Name)
	}
	if rawVersion == "" {
		return "", release.HTMLURL, errors.New("latest OVMS version is unavailable")
	}

	parsed := extractVersion(rawVersion)
	if parsed == "" {
		parsed = rawVersion
	}

	return parsed, strings.TrimSpace(release.HTMLURL), nil
}

func extractVersion(value string) string {
	match := versionRegex.FindString(strings.TrimSpace(value))
	return strings.TrimSpace(match)
}

func compareVersionStrings(installedVersion string, latestVersion string) (int, error) {
	installedParts, err := parseVersionTriplet(installedVersion)
	if err != nil {
		return 0, fmt.Errorf("cannot parse installed version: %w", err)
	}
	latestParts, err := parseVersionTriplet(latestVersion)
	if err != nil {
		return 0, fmt.Errorf("cannot parse latest version: %w", err)
	}

	for idx := 0; idx < len(installedParts); idx++ {
		if installedParts[idx] < latestParts[idx] {
			return -1, nil
		}
		if installedParts[idx] > latestParts[idx] {
			return 1, nil
		}
	}

	return 0, nil
}

func parseVersionTriplet(value string) ([3]int, error) {
	match := versionTriplet.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) != 4 {
		return [3]int{}, fmt.Errorf("version %q does not contain MAJOR.MINOR", value)
	}

	major, err := strconv.Atoi(match[1])
	if err != nil {
		return [3]int{}, err
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return [3]int{}, err
	}
	patch := 0
	if strings.TrimSpace(match[3]) != "" {
		parsedPatch, err := strconv.Atoi(match[3])
		if err != nil {
			return [3]int{}, err
		}
		patch = parsedPatch
	}

	return [3]int{major, minor, patch}, nil
}

func appendError(existingError string, newError string) string {
	if existingError == "" {
		return newError
	}
	if newError == "" {
		return existingError
	}
	return existingError + "; " + newError
}

func formatOVMSInstallError(runErr error, output string) string {
	text := strings.ToLower(strings.TrimSpace(output))
	if strings.Contains(text, "createservice failed 1073") || strings.Contains(text, "service already exists") {
		return "OVMS service already exists. Installation step skipped."
	}
	if strings.Contains(text, "access is denied") || strings.Contains(text, "openscmanager failed 5") {
		return "OVMS service install requires Administrator privileges. Run the app from an elevated terminal and retry."
	}
	if runErr == nil {
		return "OVMS service installation failed"
	}
	return runErr.Error()
}

func formatOVMSServiceActionError(action string, runErr error, output string) string {
	text := strings.ToLower(strings.TrimSpace(output))
	if strings.Contains(text, "access is denied") || strings.Contains(text, "failed 5") {
		return fmt.Sprintf("OVMS service %s requires Administrator privileges. Run this app as Administrator and retry.", action)
	}
	if strings.Contains(text, "failed 1060") || strings.Contains(text, "does not exist") {
		return "OVMS service is not installed. Install service first."
	}
	if runErr == nil {
		return fmt.Sprintf("OVMS service %s failed", action)
	}
	return runErr.Error()
}

func isOVMSServiceAlreadyInstalledOutput(output string) bool {
	text := strings.ToLower(strings.TrimSpace(output))
	return strings.Contains(text, "createservice failed 1073") || strings.Contains(text, "service already exists")
}

func parseCatalogPagination(r *http.Request) (int, int) {
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("pageSize"), 20)
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

func parsePositiveInt(value string, defaultValue int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

// checkGGUFOpenVINOCompat returns whether a GGUF model is likely compatible with
// OpenVINO GenAI serving. It uses the HF pipeline_tag and model tags to decide.
func checkGGUFOpenVINOCompat(task string, tags []string) (bool, string) {
	normTask := strings.ToLower(strings.TrimSpace(task))

	// Tasks that OpenVINO GenAI can serve via GGUF
	compatTasks := map[string]bool{
		"text-generation":      true,
		"text2text-generation": true,
		"conversational":       true,
		"question-answering":   true,
		"summarization":        true,
		"translation":          true,
		"feature-extraction":   true,
		"sentence-similarity":  true,
		"fill-mask":            true,
	}

	// Tasks that are definitely NOT compatible
	incompatTasks := map[string]bool{
		"text-to-image":                  true,
		"image-to-image":                 true,
		"image-classification":           true,
		"image-text-to-text":             true,
		"visual-question-answering":      true,
		"zero-shot-image-classification": true,
		"object-detection":               true,
		"image-segmentation":             true,
		"depth-estimation":               true,
		"automatic-speech-recognition":   true,
		"audio-classification":           true,
		"text-to-audio":                  true,
		"text-to-speech":                 true,
		"audio-to-audio":                 true,
		"video-classification":           true,
		"unconditional-image-generation": true,
		"mask-generation":                true,
		"keypoint-detection":             true,
	}

	if incompatTasks[normTask] {
		return false, "task '" + task + "' is not supported by OpenVINO GenAI GGUF serving"
	}

	// Check if any tag indicates an incompatible modality
	for _, tag := range tags {
		t := strings.ToLower(strings.TrimSpace(tag))
		if strings.HasPrefix(t, "text-to-image") || t == "diffusers" || t == "stable-diffusion" ||
			t == "stable-diffusion-xl" || t == "flux" || t == "audio" || t == "speech" ||
			t == "image-classification" || t == "object-detection" {
			return false, "model tags suggest a non-LLM modality incompatible with OpenVINO GenAI"
		}
	}

	if compatTasks[normTask] {
		return true, ""
	}

	// Empty or unknown pipeline_tag — check tags for known LLM architectures
	if normTask == "" {
		llmArchs := []string{"llama", "mistral", "phi", "qwen", "gemma", "falcon", "mpt",
			"bloom", "gpt", "opt", "llm", "causal-lm", "decoder", "mixtral", "deepseek",
			"command", "internlm", "baichuan", "chatglm", "vicuna", "wizard"}
		for _, tag := range tags {
			tl := strings.ToLower(tag)
			for _, arch := range llmArchs {
				if strings.Contains(tl, arch) {
					return true, ""
				}
			}
		}
		return false, "no pipeline_tag set — cannot confirm OpenVINO GenAI compatibility"
	}

	// Unknown task not in compat or incompat list
	return false, "task '" + task + "' is not a known OpenVINO GenAI supported task"
}

func normalizeOVMSTask(value string) string {
	cleaned := strings.TrimSpace(strings.ToLower(value))
	cleaned = strings.ReplaceAll(cleaned, "-", "_")
	cleaned = strings.ReplaceAll(cleaned, " ", "_")

	switch cleaned {
	case "", "none", "na", "n_a":
		return ""
	case "text_generation", "text2text_generation", "conversational":
		return "text_generation"
	case "feature_extraction", "sentence_similarity", "text_embedding", "embeddings":
		return "embeddings"
	case "rerank", "reranker", "text_ranking", "rank":
		return "rerank"
	case "image_generation", "text_to_image":
		return "image_generation"
	default:
		return cleaned
	}
}

func inferOVMSTaskFromSource(sourceModel string) string {
	value := strings.ToLower(strings.TrimSpace(sourceModel))

	if strings.Contains(value, "rerank") || strings.Contains(value, "reranker") || strings.Contains(value, "cross-encoder") {
		return "rerank"
	}
	if strings.Contains(value, "embed") || strings.Contains(value, "embedding") || strings.Contains(value, "sentence") || strings.Contains(value, "e5") || strings.Contains(value, "bge") {
		return "embeddings"
	}
	if strings.Contains(value, "stable-diffusion") || strings.Contains(value, "image") || strings.Contains(value, "flux") || strings.Contains(value, "sdxl") {
		return "image_generation"
	}

	return "text_generation"
}

func (a *app) listCatalogFromInternet(searchQuery string, page int, pageSize int, source string, fetchOffset int) (catalogResponse, error) {
	fetchLimit := a.catalogLimit
	if fetchLimit <= 0 {
		fetchLimit = 200
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if page <= 0 {
		page = 1
	}

	params := url.Values{}
	if source == "gguf" {
		params.Set("tags", "gguf")
	} else {
		params.Set("author", "OpenVINO")
	}
	params.Set("limit", strconv.Itoa(fetchLimit))
	params.Set("sort", "downloads")
	params.Set("direction", "-1")
	if fetchOffset > 0 {
		params.Set("offset", strconv.Itoa(fetchOffset))
	}
	if searchQuery != "" {
		params.Set("search", searchQuery)
	}

	endpoint := "https://huggingface.co/api/models?" + params.Encode()
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return catalogResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ovms-manager-win/1.0")

	client := &http.Client{Timeout: 25 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return catalogResponse{}, fmt.Errorf("catalog fetch failed: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return catalogResponse{}, fmt.Errorf("catalog fetch failed with status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var models []hfCatalogModel
	if err := json.NewDecoder(res.Body).Decode(&models); err != nil {
		return catalogResponse{}, fmt.Errorf("catalog response parse failed: %w", err)
	}

	hasMore := len(models) >= fetchLimit
	isGGUF := source == "gguf"
	items := make([]catalogItem, 0, len(models))
	for _, model := range models {
		task := strings.TrimSpace(model.PipelineTag)
		suggestedTask := normalizeOVMSTask(task)
		if isGGUF {
			compatible, _ := checkGGUFOpenVINOCompat(task, model.Tags)
			if !compatible {
				continue
			}
		}
		items = append(items, catalogItem{
			ID:            model.ID,
			Downloads:     model.Downloads,
			Likes:         model.Likes,
			Task:          task,
			SuggestedTask: suggestedTask,
			LastModified:  model.LastModified,
			Tags:          model.Tags,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Downloads == items[j].Downloads {
			return items[i].ID < items[j].ID
		}
		return items[i].Downloads > items[j].Downloads
	})

	total := len(items)
	totalPages := 1
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	if page > totalPages {
		page = totalPages
	}
	if page <= 0 {
		page = 1
	}

	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	return catalogResponse{
		Items:       items[start:end],
		Page:        page,
		PageSize:    pageSize,
		Total:       total,
		TotalPages:  totalPages,
		HasMore:     hasMore,
		FetchOffset: fetchOffset,
	}, nil
}

func (a *app) enqueueDownload(req downloadRequest) downloadJob {
	now := time.Now().UTC()
	jobID := fmt.Sprintf("job-%d-%d", now.Unix(), atomic.AddInt64(&a.jobCounter, 1))
	job := &downloadJob{
		ID:           jobID,
		SourceModel:  req.SourceModel,
		ModelName:    req.ModelName,
		TargetDevice: req.TargetDevice,
		Task:         req.Task,
		Status:       "queued",
		Progress:     0,
		ProgressText: "Queued",
		CreatedAt:    now.Format(time.RFC3339),
	}

	a.mu.Lock()
	a.downloadJobs[jobID] = job
	a.mu.Unlock()

	go a.processDownloadJob(jobID, req)
	return *job
}

func (a *app) processDownloadJob(jobID string, req downloadRequest) {
	started := time.Now().UTC().Format(time.RFC3339)
	a.mu.Lock()
	if job, ok := a.downloadJobs[jobID]; ok {
		job.Status = "running"
		job.Progress = 5
		job.ProgressText = "Preparing download"
		job.StartedAt = started
	}
	a.mu.Unlock()

	output, err := a.downloadModel(jobID, req)
	finished := time.Now().UTC().Format(time.RFC3339)

	a.mu.Lock()
	defer a.mu.Unlock()
	job, ok := a.downloadJobs[jobID]
	if !ok {
		return
	}
	job.Output = truncateText(output, 6000)
	job.FinishedAt = finished
	if err != nil {
		job.Status = "failed"
		if job.Progress < 100 {
			job.Progress = 100
		}
		if job.ProgressText == "" {
			job.ProgressText = "Failed"
		}
		job.Error = err.Error()
		return
	}
	job.Status = "succeeded"
	job.Progress = 100
	job.ProgressText = "Completed"
}

func (a *app) listDownloadJobs() []downloadJob {
	a.mu.Lock()
	defer a.mu.Unlock()

	jobs := make([]downloadJob, 0, len(a.downloadJobs))
	for _, job := range a.downloadJobs {
		jobs = append(jobs, *job)
	}

	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt == jobs[j].CreatedAt {
			return jobs[i].ID > jobs[j].ID
		}
		return jobs[i].CreatedAt > jobs[j].CreatedAt
	})

	if len(jobs) > 20 {
		jobs = jobs[:20]
	}
	return jobs
}

func (a *app) downloadModel(jobID string, req downloadRequest) (string, error) {
	ovmsPath, err := a.resolveOVMSExecutablePath()
	if err != nil {
		return "", err
	}

	attempts := []downloadRequest{req}
	if req.Task != "text_generation" {
		fallback := req
		fallback.Task = "text_generation"
		attempts = append(attempts, fallback)
	}

	var allOutput strings.Builder
	var lastErr error

	for idx, attempt := range attempts {
		startProgress, endProgress := mapAttemptProgressWindow(idx, len(attempts))
		attemptLabel := fmt.Sprintf("Downloading (attempt %d/%d)", idx+1, len(attempts))
		a.updateDownloadJobProgress(jobID, startProgress, attemptLabel)

		output, runErr := a.runDownloadCommand(ovmsPath, attempt, func(line string) {
			text := sanitizeProgressText(line)
			if text == "" {
				return
			}

			progress := startProgress
			if percent, ok := extractProgressPercent(text); ok {
				progress = startProgress + ((endProgress-startProgress)*percent)/100
			}
			a.updateDownloadJobProgress(jobID, progress, text)
		})
		allOutput.WriteString(fmt.Sprintf("\n==== attempt %d/%d", idx+1, len(attempts)))
		if attempt.Task != "" {
			allOutput.WriteString(" task=")
			allOutput.WriteString(attempt.Task)
		} else {
			allOutput.WriteString(" task=<none>")
		}
		allOutput.WriteString(" ====\n")
		allOutput.WriteString(output)
		allOutput.WriteString("\n")

		if runErr == nil {
			a.updateDownloadJobProgress(jobID, endProgress, fmt.Sprintf("Attempt %d completed", idx+1))
			return allOutput.String(), nil
		}
		a.updateDownloadJobProgress(jobID, endProgress, fmt.Sprintf("Attempt %d failed", idx+1))
		lastErr = runErr
	}

	if lastErr == nil {
		lastErr = errors.New("download failed")
	}
	return allOutput.String(), lastErr
}

func (a *app) runDownloadCommand(ovmsPath string, req downloadRequest, onLine func(string)) (string, error) {
	args := []string{
		"--pull",
		"--source_model", req.SourceModel,
		"--model_repository_path", a.repoPath,
		"--model_name", req.ModelName,
		"--target_device", req.TargetDevice,
	}
	if req.Task != "" {
		args = append(args, "--task", req.Task)
	}
	if req.Overwrite {
		args = append(args, "--overwrite_models")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, ovmsPath, args...)
	cmd.Dir = filepath.Dir(ovmsPath)
	cmd.Env = prepareOVMSEnv(cmd.Dir, a.repoPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("download setup failed: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("download setup failed: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("download start failed: %w", err)
	}

	var outputMu sync.Mutex
	var outputBuilder strings.Builder
	consume := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			outputMu.Lock()
			outputBuilder.WriteString(line)
			outputBuilder.WriteByte('\n')
			outputMu.Unlock()
			if onLine != nil {
				onLine(line)
			}
		}
		if err := scanner.Err(); err != nil {
			outputMu.Lock()
			outputBuilder.WriteString("[scanner error] ")
			outputBuilder.WriteString(err.Error())
			outputBuilder.WriteByte('\n')
			outputMu.Unlock()
		}
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		consume(stdout)
	}()
	go func() {
		defer readers.Done()
		consume(stderr)
	}()

	err = cmd.Wait()
	readers.Wait()

	result := outputBuilder.String()
	if onLine != nil && strings.TrimSpace(result) != "" {
		onLine(strings.TrimSpace(result))
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, errors.New("download timed out")
	}
	if err != nil {
		return result, fmt.Errorf("download failed: %w", err)
	}
	return result, nil
}

func (a *app) updateDownloadJobProgress(jobID string, progress int, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	job, ok := a.downloadJobs[jobID]
	if !ok {
		return
	}

	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	if progress > job.Progress {
		job.Progress = progress
	}

	trimmed := sanitizeProgressText(text)
	if trimmed != "" {
		job.ProgressText = trimmed
	}
}

func mapAttemptProgressWindow(attemptIdx int, totalAttempts int) (int, int) {
	if totalAttempts <= 1 {
		return 10, 95
	}
	if attemptIdx <= 0 {
		return 10, 55
	}
	return 60, 95
}

func extractProgressPercent(text string) (int, bool) {
	match := downloadProgressRe.FindStringSubmatch(text)
	if len(match) < 2 {
		return 0, false
	}

	parsed, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	if parsed < 0 {
		parsed = 0
	}
	if parsed > 100 {
		parsed = 100
	}
	return parsed, true
}

func sanitizeProgressText(text string) string {
	cleaned := strings.TrimSpace(strings.ReplaceAll(text, "\r", ""))
	if cleaned == "" {
		return ""
	}
	if len(cleaned) > 180 {
		return cleaned[:180] + "..."
	}
	return cleaned
}

func (a *app) resolveOVMSExecutablePath() (string, error) {
	candidate := strings.TrimSpace(a.ovmsExePath)
	if candidate == "" {
		return "", errors.New("ovms executable path is empty")
	}

	candidates := make([]string, 0, 3)
	if filepath.IsAbs(candidate) {
		candidates = append(candidates, candidate)
	} else {
		cwd, _ := os.Getwd()
		if cwd != "" {
			candidates = append(candidates, filepath.Join(cwd, candidate))
		}
		exePath, err := os.Executable()
		if err == nil {
			exeDir := filepath.Dir(exePath)
			candidates = append(candidates, filepath.Join(exeDir, candidate))
		}
		candidates = append(candidates, candidate)
	}

	for _, path := range candidates {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			absPath, absErr := filepath.Abs(path)
			if absErr == nil {
				return absPath, nil
			}
			return path, nil
		}
	}

	return "", fmt.Errorf("ovms executable not found at %s", a.ovmsExePath)
}

func prepareOVMSEnv(ovmsDir string, repoPath string) []string {
	env := append([]string{}, os.Environ()...)
	pythonHome := filepath.Join(ovmsDir, "python")
	pythonScripts := filepath.Join(pythonHome, "Scripts")
	currentPath := os.Getenv("PATH")
	prepend := strings.Join([]string{ovmsDir, pythonHome, pythonScripts}, string(os.PathListSeparator))
	if currentPath != "" {
		prepend = prepend + string(os.PathListSeparator) + currentPath
	}

	env = upsertEnv(env, "OVMS_MODEL_REPOSITORY_PATH", repoPath)
	env = upsertEnv(env, "PYTHONHOME", pythonHome)
	env = upsertEnv(env, "PATH", prepend)
	return env
}

func upsertEnv(env []string, key string, value string) []string {
	prefix := key + "="
	for idx, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[idx] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

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

func truncateText(text string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if len(text) <= maxChars {
		return text
	}
	return text[:maxChars] + "\n...(truncated)"
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

package main

import (
	"errors"
	"regexp"
	"sync"
)

// ── Compiled regexes ──────────────────────────────────────────────────────────

var (
	modelNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
	serviceStateRegex  = regexp.MustCompile(`(?m)STATE\s*:\s*\d+\s+([A-Z_]+)`)
	versionRegex       = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:[-+._][0-9A-Za-z]+)?`)
	ggufModelsDirRegex = regexp.MustCompile(`(?m)(^\s*models_path\s*:\s*)"(?:\./|\.)",?`)
	versionTriplet     = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)
	downloadProgressRe = regexp.MustCompile(`(\d{1,3})\s*%`)
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	ovmsServiceName             = "ovms"
	ovmsServingArtifactsDirName = "_ovms_serving"
	ovmsOpenAIModelsURL         = "http://127.0.0.1:8000/v3/models"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

var (
	errDownloadedModelNotFound = errors.New("downloaded model not found")
	errModelAlreadyRegistered  = errors.New("model already registered")
	errModelNotRegistered      = errors.New("model is not registered")
	errModelIncompatible       = errors.New("model is not compatible with OVMS")
)

// ── Application state ─────────────────────────────────────────────────────────

type app struct {
	repoPath     string
	configPath   string
	ovmsExePath  string
	catalogLimit int
	jobCounter   int64
	downloadJobs map[string]*downloadJob
	mu           sync.Mutex
}

// ── Config file types ─────────────────────────────────────────────────────────

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

// ── Model types ───────────────────────────────────────────────────────────────

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

// ── Catalog types ─────────────────────────────────────────────────────────────

type catalogItem struct {
	ID            string   `json:"id"`
	Downloads     int      `json:"downloads"`
	Likes         int      `json:"likes"`
	Task          string   `json:"task,omitempty"`
	SuggestedTask string   `json:"suggestedTask,omitempty"`
	LastModified  string   `json:"lastModified,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Params        int64    `json:"params,omitempty"`
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

// ── Download types ────────────────────────────────────────────────────────────

type downloadRequest struct {
	SourceModel  string `json:"sourceModel"`
	ModelName    string `json:"modelName"`
	TargetDevice string `json:"targetDevice"`
	Task         string `json:"task"`
	Overwrite    bool   `json:"overwrite"`
}

type hfSafetensors struct {
	Total int64 `json:"total"`
}

type hfCatalogModel struct {
	ID           string         `json:"id"`
	Downloads    int            `json:"downloads"`
	Likes        int            `json:"likes"`
	PipelineTag  string         `json:"pipeline_tag"`
	LastModified string         `json:"lastModified"`
	UpdatedAt    string         `json:"updatedAt"`
	Tags         []string       `json:"tags"`
	Safetensors  *hfSafetensors `json:"safetensors"`
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

// ── OVMS status types ─────────────────────────────────────────────────────────

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

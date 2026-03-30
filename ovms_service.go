package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ── OVMS status ───────────────────────────────────────────────────────────────

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

// ── Service install / control ─────────────────────────────────────────────────

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

// ── Version detection ────────────────────────────────────────────────────────

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

// ── Version comparison ────────────────────────────────────────────────────────

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

// ── Error formatting helpers ──────────────────────────────────────────────────

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

// ── OVMS executable + environment ─────────────────────────────────────────────

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

// ── OVMS model load polling ───────────────────────────────────────────────────

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

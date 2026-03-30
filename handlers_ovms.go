package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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

// handleOVMSLogs streams the tail of the OVMS log file as JSON.
// Query params: lines (int, default 300, max 5000), errors (bool, filter to errors/warnings only).
func (a *app) handleOVMSLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines <= 0 {
		lines = 300
	}
	if lines > 5000 {
		lines = 5000
	}
	errorsOnly := r.URL.Query().Get("errors") == "true"

	logPath, err := a.resolveOVMSLogPath()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	content, err := readLastLines(logPath, lines)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]string{"content": "(log file not found)"})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if errorsOnly {
		content = filterLogErrors(content)
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": content})
}

// resolveOVMSLogPath returns the path to the OVMS server log file.
func (a *app) resolveOVMSLogPath() (string, error) {
	ovmsPath, err := a.resolveOVMSExecutablePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(ovmsPath), "ovms_server.log"), nil
}

// readLastLines reads the last n lines from a file, reading at most 512 KB from
// the end to keep memory usage bounded on large log files.
func readLastLines(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	const maxRead = 512 * 1024
	offset := info.Size() - maxRead
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}

	allLines := strings.Split(string(data), "\n")
	if len(allLines) > n {
		allLines = allLines[len(allLines)-n:]
	}
	return strings.Join(allLines, "\n"), nil
}

// filterLogErrors keeps only lines that contain error, warning, or exception keywords.
func filterLogErrors(content string) string {
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "[error]") || strings.Contains(lower, "[warn]") ||
			strings.Contains(lower, "exception") || strings.Contains(lower, "failed") ||
			strings.Contains(lower, "could not") {
			filtered = append(filtered, line)
		}
	}
	if len(filtered) == 0 {
		return "(no errors or warnings found in the selected log range)"
	}
	return strings.Join(filtered, "\n")
}

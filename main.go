package main

import (
"embed"
"encoding/json"
"flag"
"fmt"
"io/fs"
"log"
"net/http"
"path/filepath"
"strings"
)

//go:embed web/*
var webFS embed.FS

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
mux.HandleFunc("/api/ovms/logs", instance.handleOVMSLogs)

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
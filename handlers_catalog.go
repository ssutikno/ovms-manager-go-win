package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

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

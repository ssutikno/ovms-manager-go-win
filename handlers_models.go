package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"
)

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

	writeJSON(w, http.StatusOK, map[string]string{"message": fmt.Sprintf("model %q unregistered", req.Name)})
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

	wasRegistered, err := a.deleteDownloadedModel(req.Name)
	if err != nil {
		if errors.Is(err, errDownloadedModelNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	msg := fmt.Sprintf("model %q deleted", req.Name)
	if wasRegistered {
		msg += " and unregistered from OVMS config"
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
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

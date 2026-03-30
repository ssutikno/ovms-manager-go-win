package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

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
		lastMod := model.LastModified
		if lastMod == "" {
			lastMod = model.UpdatedAt
		}
		var params int64
		if model.Safetensors != nil {
			params = model.Safetensors.Total
		}
		items = append(items, catalogItem{
			ID:            model.ID,
			Downloads:     model.Downloads,
			Likes:         model.Likes,
			Task:          task,
			SuggestedTask: suggestedTask,
			LastModified:  lastMod,
			Tags:          model.Tags,
			Params:        params,
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

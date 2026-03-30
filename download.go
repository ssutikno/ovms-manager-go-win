package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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

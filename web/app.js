const catalogBody = document.getElementById("catalog-body");
const catalogQueryInput = document.getElementById("catalog-query");
const catalogSearchButton = document.getElementById("catalog-search-btn");
const catalogPrevButton = document.getElementById("catalog-prev-btn");
const catalogNextButton = document.getElementById("catalog-next-btn");
const catalogLoadMoreButton = document.getElementById("catalog-loadmore-btn");
const catalogSourceOpenVINO = document.getElementById("catalog-source-openvino");
const catalogSourceGGUF = document.getElementById("catalog-source-gguf");
const catalogPageInfo = document.getElementById("catalog-page-info");
const jobsBody = document.getElementById("jobs-body");
const jobsRefreshButton = document.getElementById("jobs-refresh-btn");
const downloadedBody = document.getElementById("downloaded-body");
const statusElement = document.getElementById("status");
const refreshButton = document.getElementById("refresh-btn");
const ovmsRefreshButton = document.getElementById("ovms-refresh-btn");
const ovmsInstallButton = document.getElementById("ovms-install-btn");
const ovmsStartButton = document.getElementById("ovms-start-btn");
const ovmsStopButton = document.getElementById("ovms-stop-btn");
const ovmsUpdateButton = document.getElementById("ovms-update-btn");
const ovmsServiceState = document.getElementById("ovms-service-state");
const ovmsInstalledVersion = document.getElementById("ovms-installed-version");
const ovmsLatestVersion = document.getElementById("ovms-latest-version");
const ovmsUpdateState = document.getElementById("ovms-update-state");
const ovmsDetails = document.getElementById("ovms-details");
const ovmsLatestLink = document.getElementById("ovms-latest-link");

let catalogPage = 1;
const catalogPageSize = 20;
let catalogTotalPages = 1;
let catalogSource = "openvino";
let catalogFetchOffset = 0;
const catalogFetchLimit = 200;
let jobsPollingHandle = null;

refreshButton.addEventListener("click", () => refreshAll(true));
catalogSearchButton.addEventListener("click", () => {
  catalogPage = 1;
  catalogFetchOffset = 0;
  loadCatalog(true);
});
catalogPrevButton.addEventListener("click", () => {
  if (catalogPage > 1) {
    catalogPage -= 1;
    loadCatalog(true);
  }
});
catalogNextButton.addEventListener("click", () => {
  if (catalogPage < catalogTotalPages) {
    catalogPage += 1;
    loadCatalog(true);
  }
});
jobsRefreshButton.addEventListener("click", () => loadDownloadJobs(true));
ovmsRefreshButton.addEventListener("click", () => loadOVMSStatus(true));
ovmsInstallButton.addEventListener("click", () => installOVMSService());
ovmsStartButton.addEventListener("click", () => startOVMSService());
ovmsStopButton.addEventListener("click", () => stopOVMSService());
ovmsUpdateButton.addEventListener("click", () => checkOVMSUpdate());
catalogQueryInput.addEventListener("keydown", (event) => {
  if (event.key === "Enter") {
    catalogPage = 1;
    catalogFetchOffset = 0;
    loadCatalog(true);
  }
});

catalogSourceOpenVINO.addEventListener("click", () => {
  if (catalogSource !== "openvino") {
    catalogSource = "openvino";
    catalogPage = 1;
    catalogFetchOffset = 0;
    updateSourceButtons();
    loadCatalog(true);
  }
});

catalogSourceGGUF.addEventListener("click", () => {
  if (catalogSource !== "gguf") {
    catalogSource = "gguf";
    catalogPage = 1;
    catalogFetchOffset = 0;
    updateSourceButtons();
    loadCatalog(true);
  }
});

catalogLoadMoreButton.addEventListener("click", () => {
  catalogFetchOffset += catalogFetchLimit;
  catalogPage = 1;
  loadCatalog(true);
});

function setStatus(message, isError = false) {
  statusElement.textContent = message;
  statusElement.className = isError ? "status error" : "status";
}

async function request(url, options = {}) {
  const response = await fetch(url, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });

  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(payload.error || `Request failed: ${response.status}`);
  }
  return payload;
}

function createCell(text) {
  const td = document.createElement("td");
  td.textContent = text ?? "";
  return td;
}

function deriveModelName(sourceModel) {
  if (!sourceModel) {
    return "model";
  }
  const segments = sourceModel.split("/");
  const raw = segments[segments.length - 1] || "model";
  const normalized = raw.replace(/[^a-zA-Z0-9._-]+/g, "-").replace(/^[-._]+|[-._]+$/g, "");
  return normalized || "model";
}

function formatDownloads(value) {
  if (typeof value !== "number") {
    return "0";
  }
  return value.toLocaleString();
}

function renderOVMSStatus(status) {
  const installed = !!status.serviceInstalled;
  const running = !!status.serviceRunning;
  const state = (status.serviceState || "unknown").toLowerCase();

  let serviceText = "Unknown";
  let serviceClass = "ovms-pill";

  if (!installed) {
    serviceText = "Not Installed";
    serviceClass += " missing";
  } else if (running) {
    serviceText = "Running";
    serviceClass += " running";
  } else {
    serviceText = `Stopped (${state})`;
    serviceClass += " stopped";
  }

  ovmsServiceState.textContent = serviceText;
  ovmsServiceState.className = serviceClass;

  ovmsStartButton.disabled = !installed || running;
  ovmsStopButton.disabled = !installed || !running;

  ovmsInstalledVersion.textContent = status.installedVersion || "Unknown";
  ovmsLatestVersion.textContent = status.latestVersion || "Unknown";

  if (status.installedVersion && status.latestVersion) {
    ovmsUpdateState.textContent = status.updateAvailable ? "Update Available" : "Up To Date";
  } else {
    ovmsUpdateState.textContent = "Unknown";
  }

  const details = [];
  if (status.serviceError) {
    details.push(`Service check: ${status.serviceError}`);
  }
  if (status.installedVersionError) {
    details.push(`Installed version: ${status.installedVersionError}`);
  }
  if (status.versionCheckError) {
    details.push(`Latest version: ${status.versionCheckError}`);
  }

  ovmsDetails.textContent = details.length ? details.join(" | ") : "OVMS runtime checks look healthy.";

  if (status.latestVersionUrl) {
    ovmsLatestLink.href = status.latestVersionUrl;
    ovmsLatestLink.hidden = false;
  } else {
    ovmsLatestLink.hidden = true;
  }
}

function renderCatalog(models) {
  catalogBody.innerHTML = "";

  if (!models.length) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 5;
    cell.textContent = "No catalog models found";
    row.appendChild(cell);
    catalogBody.appendChild(row);
    return;
  }

  models.forEach((model) => {
    const row = document.createElement("tr");
    row.appendChild(createCell(model.id));
    row.appendChild(createCell(formatDownloads(model.downloads)));
    row.appendChild(createCell(model.task || ""));
    row.appendChild(createCell(model.lastModified || ""));

    const actionCell = document.createElement("td");

    const taskInput = document.createElement("input");
    taskInput.type = "text";
    taskInput.placeholder = "task (optional)";
    taskInput.value = model.suggestedTask || "";
    taskInput.className = "task-input";

    const button = document.createElement("button");
    button.textContent = "Download";
    button.addEventListener("click", async () => {
      try {
        setStatus(`Queueing download for ${model.id}...`);
        const payload = await request("/api/download", {
          method: "POST",
          body: JSON.stringify({
            sourceModel: model.id,
            modelName: deriveModelName(model.id),
            task: taskInput.value.trim(),
          }),
        });
        setStatus(`Download queued (${payload.job.id}) for ${model.id}`);
        await loadDownloadJobs(false);
      } catch (error) {
        setStatus(error.message, true);
      }
    });

    actionCell.appendChild(taskInput);
    actionCell.appendChild(button);

    row.appendChild(actionCell);
    catalogBody.appendChild(row);
  });
}

function renderDownloadJobs(jobs) {
  jobsBody.innerHTML = "";

  if (!jobs.length) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 7;
    cell.textContent = "No download jobs";
    row.appendChild(cell);
    jobsBody.appendChild(row);
    return;
  }

  jobs.forEach((job) => {
    const row = document.createElement("tr");
    row.appendChild(createCell(job.id));
    row.appendChild(createCell(job.sourceModel));
    row.appendChild(createCell(job.modelName));
    row.appendChild(createCell(job.status));
    row.appendChild(createCell(formatJobProgress(job)));
    row.appendChild(createCell(job.task || ""));
    row.appendChild(createCell(job.error || ""));
    jobsBody.appendChild(row);
  });
}

function formatJobProgress(job) {
  const numeric = Number.isFinite(job.progress) ? Math.max(0, Math.min(100, Math.trunc(job.progress))) : null;
  const text = (job.progressText || "").trim();

  if (numeric === null && !text) {
    return "-";
  }
  if (numeric === null) {
    return text;
  }
  if (!text) {
    return `${numeric}%`;
  }
  return `${numeric}% - ${text}`;
}

function renderDownloadedModels(models) {
  downloadedBody.innerHTML = "";

  if (!models.length) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 5;
    cell.textContent = "No downloaded models";
    row.appendChild(cell);
    downloadedBody.appendChild(row);
    return;
  }

  models.forEach((model) => {
    const row = document.createElement("tr");
    row.appendChild(createCell(model.name));
    row.appendChild(createCell(model.compatible ? "Yes" : "No"));
    row.appendChild(createCell(model.reason || ""));
    row.appendChild(createCell(model.registered ? `Registered (${model.targetDevice || "CPU"})` : "Not Registered"));

    const actionCell = document.createElement("td");

    const deleteButton = document.createElement("button");
    deleteButton.className = "danger";
    deleteButton.textContent = "Delete";
    deleteButton.addEventListener("click", async () => {
      const confirmed = window.confirm(
        `Delete ${model.name}? This will remove model files from disk${model.registered ? " and unregister it" : ""}.`
      );
      if (!confirmed) {
        return;
      }

      try {
        setStatus(`Deleting ${model.name}...`);
        const payload = await request("/api/models/delete", {
          method: "POST",
          body: JSON.stringify({ name: model.name }),
        });
        setStatus(payload.message || `Deleted ${model.name}`);
        await loadModels(false);
        await loadDownloadJobs(false);
      } catch (error) {
        setStatus(error.message, true);
      }
    });

    if (model.registered) {
      const button = document.createElement("button");
      button.className = "danger";
      button.textContent = "Unregister";
      button.addEventListener("click", async () => {
        try {
          setStatus(`Unregistering ${model.name}...`);
          await request("/api/unregister", {
            method: "POST",
            body: JSON.stringify({ name: model.name }),
          });
          setStatus(`Unregistered ${model.name}`);
          await loadModels(false);
        } catch (error) {
          setStatus(error.message, true);
        }
      });

      actionCell.appendChild(button);
      actionCell.appendChild(deleteButton);
    } else {
      const select = document.createElement("select");
      ["CPU", "GPU", "AUTO", "MULTI", "HETERO"].forEach((value) => {
        const option = document.createElement("option");
        option.value = value;
        option.textContent = value;
        select.appendChild(option);
      });

      if (model.targetDevice) {
        select.value = model.targetDevice;
      }

      const button = document.createElement("button");
      button.textContent = "Register";
      button.addEventListener("click", async () => {
        try {
          if (!model.compatible) {
            setStatus(`Cannot register ${model.name}: ${model.reason || "model is not compatible with OVMS"}`, true);
            return;
          }

          setStatus(`Registering ${model.name}...`);
          const payload = await request("/api/register", {
            method: "POST",
            body: JSON.stringify({
              name: model.name,
              targetDevice: select.value,
            }),
          });
          setStatus(payload.message || `Registered ${model.name}`);
          await loadModels(false);
        } catch (error) {
          setStatus(error.message, true);
        }
      });

      actionCell.appendChild(select);
      actionCell.appendChild(button);
      actionCell.appendChild(deleteButton);
    }

    row.appendChild(actionCell);
    downloadedBody.appendChild(row);
  });
}

async function loadModels(showMessage = true) {
  try {
    if (showMessage) {
      setStatus("Loading models...");
    }

    const payload = await request("/api/models");
    renderDownloadedModels(payload.downloadedModels || []);

    if (showMessage) {
      setStatus("Model list updated");
    }
  } catch (error) {
    setStatus(error.message, true);
  }
}

async function loadOVMSStatus(showMessage = true) {
  try {
    if (showMessage) {
      setStatus("Loading OVMS runtime status...");
    }

    const payload = await request("/api/ovms/status");
    renderOVMSStatus(payload || {});

    if (showMessage) {
      setStatus("OVMS runtime status updated");
    }
  } catch (error) {
    setStatus(error.message, true);
  }
}

async function installOVMSService() {
  try {
    setStatus("Installing OVMS service...");
    const payload = await request("/api/ovms/install", { method: "POST" });
    if (payload.status) {
      renderOVMSStatus(payload.status);
    }
    if (payload.output) {
      ovmsDetails.textContent = payload.output;
    }
    if (payload.success) {
      setStatus(payload.message || "OVMS service installation completed");
    } else {
      setStatus(payload.message || "OVMS service installation failed", true);
    }
  } catch (error) {
    setStatus(error.message, true);
  }
}

async function startOVMSService() {
  try {
    setStatus("Starting OVMS service...");
    const payload = await request("/api/ovms/start", { method: "POST" });
    if (payload.status) {
      renderOVMSStatus(payload.status);
    }
    if (payload.output) {
      ovmsDetails.textContent = payload.output;
    }
    if (payload.success) {
      setStatus(payload.message || "OVMS service started");
    } else {
      setStatus(payload.message || "OVMS service start failed", true);
    }
  } catch (error) {
    setStatus(error.message, true);
  }
}

async function stopOVMSService() {
  try {
    setStatus("Stopping OVMS service...");
    const payload = await request("/api/ovms/stop", { method: "POST" });
    if (payload.status) {
      renderOVMSStatus(payload.status);
    }
    if (payload.output) {
      ovmsDetails.textContent = payload.output;
    }
    if (payload.success) {
      setStatus(payload.message || "OVMS service stopped");
    } else {
      setStatus(payload.message || "OVMS service stop failed", true);
    }
  } catch (error) {
    setStatus(error.message, true);
  }
}

async function checkOVMSUpdate() {
  try {
    setStatus("Checking OVMS latest version...");
    const payload = await request("/api/ovms/update", { method: "POST" });
    if (payload.status) {
      renderOVMSStatus(payload.status);
    }
    if (payload.output) {
      ovmsDetails.textContent = payload.output;
    }
    if (payload.success) {
      setStatus(payload.message || "OVMS update check completed");
    } else {
      setStatus(payload.message || "OVMS update check completed with warnings", true);
    }
  } catch (error) {
    setStatus(error.message, true);
  }
}

async function loadCatalog(showMessage = true) {
  try {
    if (showMessage) {
      setStatus("Loading model catalog...");
    }

    const query = encodeURIComponent(catalogQueryInput.value.trim());
    const payload = await request(
      `/api/catalog?q=${query}&page=${catalogPage}&pageSize=${catalogPageSize}&source=${catalogSource}&offset=${catalogFetchOffset}`
    );
    renderCatalog(payload.items || []);
    catalogPage = payload.page || 1;
    catalogTotalPages = payload.totalPages || 1;
    const batchLabel = catalogFetchOffset > 0 ? ` (#${catalogFetchOffset + 1}+)` : "";
    catalogPageInfo.textContent = `Page ${catalogPage} / ${catalogTotalPages}${batchLabel} (${payload.total || 0} this batch)`;
    catalogPrevButton.disabled = catalogPage <= 1;
    catalogNextButton.disabled = catalogPage >= catalogTotalPages;
    catalogLoadMoreButton.disabled = !payload.hasMore;

    if (showMessage) {
      setStatus("Catalog updated");
    }
  } catch (error) {
    setStatus(error.message, true);
  }
}

function updateSourceButtons() {
  catalogSourceOpenVINO.classList.toggle("active", catalogSource === "openvino");
  catalogSourceGGUF.classList.toggle("active", catalogSource === "gguf");
}

async function loadDownloadJobs(showMessage = false) {
  try {
    if (showMessage) {
      setStatus("Loading download jobs...");
    }

    const payload = await request("/api/downloads");
    const jobs = payload.jobs || [];
    renderDownloadJobs(jobs);

    const hasRunning = jobs.some((job) => job.status === "queued" || job.status === "running");
    if (hasRunning) {
      await loadModels(false);
    }

    if (showMessage) {
      setStatus("Download jobs updated");
    }
  } catch (error) {
    setStatus(error.message, true);
  }
}

async function refreshAll(showMessage = true) {
  if (showMessage) {
    setStatus("Refreshing OVMS, catalog, and model state...");
  }

  await loadOVMSStatus(false);
  await loadModels(false);
  await loadCatalog(false);
  await loadDownloadJobs(false);

  if (showMessage) {
    setStatus("Data updated");
  }
}

function startJobsPolling() {
  if (jobsPollingHandle) {
    clearInterval(jobsPollingHandle);
  }
  jobsPollingHandle = setInterval(() => {
    loadDownloadJobs(false);
  }, 3000);
}

startJobsPolling();
refreshAll(true);

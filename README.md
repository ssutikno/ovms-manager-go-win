# OVMS Manager for Windows (Go)

A lightweight Go web application that provides a browser-based UI to manage [OpenVINO™ Model Server (OVMS)](https://github.com/openvinotoolkit/model_server) on Windows.

It handles the full lifecycle: browsing an online model catalog, downloading models, registering them into OVMS, and controlling the OVMS Windows service — all from a single web page.

![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go) ![Platform](https://img.shields.io/badge/platform-Windows-blue) ![OVMS](https://img.shields.io/badge/OVMS-GenAI%20%7C%20Classic-green)

---

## Features

- **Online Model Catalog** — Searches Hugging Face for OpenVINO-compatible models with pagination
- **Background Downloads** — Downloads models via `ovms.exe --pull` with live progress tracking
- **Model Registration** — Registers models into `config.json` for OVMS to serve
- **Two config modes**:
  - GenAI / Mediapipe LLM pipeline models (contain `graph.pbtxt`) → written to `mediapipe_config_list`
  - Classic versioned models (IR, ONNX, etc.) → written to `model_config_list`
- **OVMS Service Control** — Install, Start, Stop, and Update the OVMS Windows service
- **Config Hot-Reload** — Triggers `/v1/config/reload` on the running OVMS instance
- **Model Deletion** — Removes model files from disk and unregisters from config

---

## Requirements

| Requirement | Notes |
|---|---|
| Windows 10/11 | 64-bit |
| Go 1.22+ | For building from source |
| OVMS for Windows | Place the distribution in `ovms\` next to the exe, or pass `-ovmsExe` |
| Administrator privileges | Required **only** for Install / Start / Stop OVMS service |

---

## Getting Started

### 1. Build

```powershell
go build -o ovms-manager-win.exe .
```

### 2. Run

```powershell
.\ovms-manager-win.exe
```

Or with explicit paths:

```powershell
.\ovms-manager-win.exe `
  -repo   "D:\ovms_models" `
  -ovmsExe ".\ovms\ovms.exe" `
  -port   8090
```

Open **http://localhost:8090** in your browser.

### CLI Flags

| Flag | Default | Description |
|---|---|---|
| `-repo` | `.\models_repo` | Directory where models are downloaded |
| `-config` | `<repo>\config.json` | Path to the OVMS `config.json` |
| `-ovmsExe` | `ovms\ovms.exe` | Path to the `ovms.exe` executable |
| `-catalogLimit` | `200` | Max models fetched from Hugging Face before pagination |
| `-port` | `8090` | Port for the web UI and API |

---

## Directory Layout

```
ovms-manager-win.exe
ovms\                        ← OVMS distribution (not included, download separately)
  ovms.exe
  install_ovms_service.bat
  setupvars.ps1
  python\
models_repo\                 ← created automatically
  config.json                ← auto-managed by the app
  OpenVINO\
    <model-name>\            ← downloaded model files
```

---

## OVMS Service Notes

The OVMS Windows service runs `ovms.exe` on **port 8000** (OpenAI-compatible REST API).

| Operation | Admin required? |
|---|---|
| Query service status | No |
| Download / register / unregister models | No |
| **Install OVMS service** | **Yes** |
| **Start / Stop OVMS service** | **Yes** |

Run the manager exe **as Administrator** when you need to install or control the service.

---

## Model Type Detection

The app automatically routes each model to the correct OVMS config section:

| Model type | Detection | Config section | `base_path` |
|---|---|---|---|
| GenAI LLM pipeline | `graph.pbtxt` contains `HttpLLMCalculator` or `LLMCalculator` | `mediapipe_config_list` | Model root folder |
| Classic OVMS model | Numeric version subdirectory (`1/`, `2/`, …) with IR/ONNX/etc. | `model_config_list` | `_ovms_serving\` subfolder |

### Classic model compatibility formats

A model is considered compatible when it has at least one numeric version folder containing:

- OpenVINO IR: `.xml` + `.bin`
- ONNX: `.onnx`
- PaddlePaddle: `.pdmodel` + `.pdiparams`
- TensorFlow SavedModel: `saved_model.pb`
- TFLite: `.tflite`

---

## API Reference

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/models` | List downloaded and registered models |
| `GET` | `/api/catalog?q=&page=1&pageSize=20` | Search Hugging Face model catalog |
| `POST` | `/api/download` | Start a background model download |
| `GET` | `/api/downloads` | List recent download job statuses |
| `POST` | `/api/register` | Register a downloaded model into OVMS config |
| `POST` | `/api/unregister` | Remove a model from OVMS config |
| `POST` | `/api/models/delete` | Delete model files from disk |
| `POST` | `/api/reload` | Trigger OVMS config hot-reload |
| `GET` | `/api/ovms/status` | Get OVMS service status and version info |
| `POST` | `/api/ovms/install` | Install the OVMS Windows service *(admin)* |
| `POST` | `/api/ovms/start` | Start the OVMS service *(admin)* |
| `POST` | `/api/ovms/stop` | Stop the OVMS service *(admin)* |
| `POST` | `/api/ovms/update` | Download and install a newer OVMS release |

### Example: Download a model

```json
POST /api/download
{
  "sourceModel": "OpenVINO/Qwen3-0.6B-int4-ov",
  "modelName":   "Qwen3-0.6B-int4-ov",
  "targetDevice": "CPU",
  "task": "text_generation"
}
```

### Example: Test inference (after model is registered and OVMS is running)

```powershell
$body = '{"model":"Qwen3-0.6B-int4-ov","messages":[{"role":"user","content":"Hello!"}],"max_tokens":100}'
Invoke-RestMethod -Uri "http://127.0.0.1:8000/v3/chat/completions" `
  -Method POST -Body $body -ContentType "application/json"
```

---

## License

This project is provided as-is. The bundled OVMS distribution is subject to the [Apache 2.0 License](ovms/thirdparty-licenses/) and Intel's terms.


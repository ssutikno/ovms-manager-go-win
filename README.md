# OVMS Manager (Go)

Simple Go web app for model management with two states:

- **Downloaded Models**: models present on disk but not in `model_config_list`
- **Registered Models**: models with entries in `config.json` (`model_config_list`)

It also includes:

- **Model Catalog (Internet)** with pagination
- **Background Download Jobs** for model pulling

## Run

From the workspace root:

```powershell
go run . -repo D:\ovms_models -config D:\ovms_models\config.json -ovmsExe .\ovms\ovms.exe -port 8090
```

If `-repo` is not provided, the app uses `./models_repo` by default.

Open:

- `http://localhost:8090`

## OVMS compatibility check used by this app

A model is considered compatible if it has at least one numeric version folder (`1`, `2`, ...)
containing one supported format:

- OpenVINO IR: `.xml` + `.bin`
- ONNX: `.onnx`
- PaddlePaddle: `.pdmodel` + `.pdiparams`
- TensorFlow: `saved_model.pb`
- TFLite: `.tflite`

## APIs

- `GET /api/models`
- `GET /api/catalog?q=<search>&page=1&pageSize=20`
- `POST /api/download` body: `{ "sourceModel": "OpenVINO/model-id", "modelName": "local-name", "targetDevice": "CPU", "task": "text_generation" }`
- `GET /api/downloads` (latest job statuses)
- `POST /api/register` body: `{ "name": "modelName", "targetDevice": "CPU" }`
- `POST /api/unregister` body: `{ "name": "modelName" }`


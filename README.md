# Local OpenAPI Docs (Multi-Spec)

Single local application that:

- discovers specs in `../**/docs/oas/openapi.yaml`
- serves a Swagger UI page with in-browser spec switching
- auto-refreshes docs when spec files change

## Run

```bash
go run .
```

Open: `http://localhost:18080/docs`

## Environment

- `OAS_DOCS_ADDR` (default `:18080`)
- `OAS_SEARCH_ROOT` (default `..`)
- `OAS_SCAN_INTERVAL` (default `3s`)

## API Endpoints

- `GET /openapi/manifest` list discovered specs
- `GET /openapi/spec/{id}` return one YAML spec
- `GET /openapi/events` SSE stream (`spec-changed`)

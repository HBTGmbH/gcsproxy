Copy/fork of https://github.com/daichirata/gcsproxy with multiple changes:
- allow using HTTP (instead of HTTPS) for the Cloud Storage API endpoint in the Google Cloud SDK client to support collecting L7 telemetry/logs via e.g. Istio and doing TLS Origination elsewhere
- implement graceful shutdown by trapping SIGTERM/SIGINT and sleeping for a configurable duration before gracefully shutting down the net/http server
- expose Prometheus metrics (currently only Go built-in runtime metrics)
- hide implementation-specific error messages in error responses (like GCP/GCS SDK Client-generated error messages about a "bucket" not being found or an "object" not found in the bucket)
- log structured JSON instead of unstructured text messages
- use an HTTP/2 server (for better resource utilization with Istio)
- support gzip compression when client advertises it in Accept-Encoding and file extension has been configured for gzip compression
- Content-Type detection for gzipped responses (including JSON)
- support ETag response header and conditional requests via If-None-Match
- add multi-arch Docker build with linux/amd64 and linux/arm64

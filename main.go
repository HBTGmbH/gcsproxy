package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"cloud.google.com/go/storage"
	"github.com/gorilla/mux"
	"google.golang.org/api/option"
)

var (
	bind                  = flag.String("b", "0.0.0.0:8080", "Bind address")
	metricsBind           = flag.String("m", "0.0.0.0:9090", "Bind address for Prometheus /metrics endpoint")
	gcsApiEndpoint        = flag.String("e", "https://storage.googleapis.com/storage/v1/", "GCS API endpoint")
	preStopSleep          = flag.Duration("s", 0*time.Second, "Sleep duration before stopping the container after receiving SIGTERM/SIGINT")
	drainTimeout          = flag.Duration("d", 15*time.Second, "Timeout for draining idle connections before shutting down the server")
	serverShutdownTimeout = flag.Duration("t", 30*time.Second, "Timeout for gracefully shutting down the net/http server")
	gzippedResources      = flag.String("g", ".js,.json,.css,.svg,.xml", "Comma-separated list of file extensions (including the dot) that should be served gzipped")
	keepAliveTimeout      = flag.Duration("k", 20*time.Minute, "The maximum amount of time to wait for the next request before closing the socket")
)

var (
	draining             = atomic.Bool{}
	openConnectionsCount int64
)

var client *storage.Client
var logger *slog.Logger
var appName string
var appVersion string

func init() {
	lvl := new(slog.LevelVar)
	env := os.Getenv("LOG_LEVEL")
	if env == "DEBUG" {
		lvl.Set(slog.LevelDebug)
	} else if env == "WARN" {
		lvl.Set(slog.LevelWarn)
	} else if env == "ERROR" {
		lvl.Set(slog.LevelError)
	}
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lvl,
	}))
	appName = os.Getenv("APP_NAME")
	appVersion = os.Getenv("APP_VERSION")
}

func handleError(w http.ResponseWriter, bucket, object string, err error) {
	if errors.Is(err, storage.ErrObjectNotExist) {
		logger.Debug("Object not found", "bucket", bucket, "object", object)
		w.WriteHeader(http.StatusNotFound)
	} else if errors.Is(err, storage.ErrBucketNotExist) {
		logger.Debug("Bucket not found", "bucket", bucket)
		w.WriteHeader(http.StatusNotFound)
	} else {
		logger.Error("Internal server error while fetching object from bucket", "error", err, "bucket", bucket, "object", object)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func setStrHeader(w http.ResponseWriter, key string, value string) {
	if value != "" {
		w.Header().Set(key, value)
	}
}

func setIntHeader(w http.ResponseWriter, key string, value int64) {
	if value > 0 {
		w.Header().Set(key, strconv.FormatInt(value, 10))
	}
}

func setTimeHeader(w http.ResponseWriter, key string, value time.Time) {
	if !value.IsZero() {
		w.Header().Set(key, value.UTC().Format(http.TimeFormat))
	}
}

func fetchObjectAttrs(ctx context.Context, bucket, object string) (*storage.ObjectAttrs, error) {
	attrs, err := client.Bucket(bucket).Object(strings.TrimSuffix(object, "/")).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, err
		}
		return nil, err
	}
	return attrs, nil
}

func validateObjectName(object string) bool {
	if strings.Contains(object, ":") {
		return false
	}
	return true
}

func isExtensionGzippable(ext string) bool {
	arg := *gzippedResources
	split := strings.Split(arg, ",")
	for _, s := range split {
		if strings.TrimSpace(s) == ext {
			return true
		}
	}
	return false
}

// gzipResponseWriter detects the Content-Type from the first 512 bytes
type gzipResponseWriter struct {
	http.ResponseWriter
	gzipWriter *gzip.Writer
	buf        *bytes.Buffer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	// if we still haven't detected the Content-Type, buffer the response
	if w.buf != nil {
		// we only need at max 512 bytes to detect the Content-Type
		needed := max(0, 512-w.buf.Len())
		l := min(len(b), needed)
		remain := b[l:]
		w.buf.Write(b[:l]) // <- buffer the first 512 bytes
		if w.buf.Len() >= 512 {
			// try json detection first
			var b = w.buf.Bytes()
			var contentType string
			if json(&b) {
				contentType = "application/json; charset=utf-8"
			} else {
				contentType = http.DetectContentType(w.buf.Bytes())
			}
			// if we have enough bytes, detect the Content-Type
			setStrHeader(w.ResponseWriter, "Content-Type", contentType)
			// write all bytes buffered so far
			_, err := w.gzipWriter.Write(w.buf.Bytes())
			w.buf = nil
			if err != nil {
				return 0, err
			}
			// and write the remaining bytes that came with this call
			_, err = w.gzipWriter.Write(remain)
			if err != nil {
				return 0, err
			}
		}
		// we always report having written all bytes
		return len(b), nil
	}
	// if we already detected the Content-Type, just write the bytes
	return w.gzipWriter.Write(b)
}

func (w *gzipResponseWriter) Close() error {
	// check if we still have a buffered response for the Content-Type detection
	if w.buf != nil {
		// if so, just detect as best as we can from the remaining bytes
		var b = w.buf.Bytes()
		var contentType string
		if json(&b) {
			contentType = "application/json; charset=utf-8"
		} else {
			contentType = http.DetectContentType(w.buf.Bytes())
		}
		setStrHeader(w.ResponseWriter, "Content-Type", contentType)
		// write the remaining buffered bytes out
		_, err := w.gzipWriter.Write(w.buf.Bytes())
		w.buf = nil
		if err != nil {
			return err
		}
	}
	return w.gzipWriter.Close()
}

func getOrHead(w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)
	bucket := params["bucket"]
	object := params["object"]
	logger.Debug("Retrieving object", "bucket", bucket, "object", object)
	attrs, err := fetchObjectAttrs(r.Context(), bucket, object)
	if err != nil {
		handleError(w, bucket, object, err)
		return
	}
	gzipAcceptable := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	if lastStrs, ok := r.Header["If-None-Match"]; ok && len(lastStrs) > 0 {
		// The value of the ETag HTTP Header is always quoted, whereas
		// the attribute returned by the GCS API is not. We need to add
		// quotes to the ETag value to compare them.
		if attrs.Etag != "" && (lastStrs[0] == "\""+attrs.Etag+"\"" || lastStrs[0] == "W/\""+attrs.Etag+"\"") {
			// send ETag if present
			if attrs.Etag != "" {
				if gzipAcceptable && attrs.ContentEncoding != "gzip" && isExtensionGzippable(filepath.Ext(object)) {
					setStrHeader(w, "ETag", "W/\""+attrs.Etag+"\"")
				} else {
					setStrHeader(w, "ETag", "\""+attrs.Etag+"\"")
				}
			}
			// send Cache-Control if present
			setStrHeader(w, "Cache-Control", attrs.CacheControl)
			// send Last-Modified if present
			setTimeHeader(w, "Last-Modified", attrs.Updated)
			w.WriteHeader(304)
			return
		}
	} else if lastStrs, ok = r.Header["If-Modified-Since"]; ok && len(lastStrs) > 0 {
		last, err := http.ParseTime(lastStrs[0])
		if err == nil && !attrs.Updated.Truncate(time.Second).After(last) {
			// send ETag if present
			if attrs.Etag != "" {
				if gzipAcceptable && attrs.ContentEncoding != "gzip" && isExtensionGzippable(filepath.Ext(object)) {
					setStrHeader(w, "ETag", "W/\""+attrs.Etag+"\"")
				} else {
					setStrHeader(w, "ETag", "\""+attrs.Etag+"\"")
				}
			}
			// send Cache-Control if present
			setStrHeader(w, "Cache-Control", attrs.CacheControl)
			// send Last-Modified if present
			setTimeHeader(w, "Last-Modified", attrs.Updated)
			w.WriteHeader(304)
			return
		}
	}

	objr, err := client.Bucket(attrs.Bucket).Object(attrs.Name).ReadCompressed(gzipAcceptable).NewReader(r.Context())
	if err != nil {
		handleError(w, bucket, object, err)
		return
	}
	defer func() {
		err := objr.Close()
		if err != nil {
			logger.Error("Failed to close reader", "error", err, "bucket", bucket, "object", object)
		}
	}()
	if appName != "" && appVersion != "" {
		setStrHeader(w, "X-Service-Version", appName+"="+appVersion)
	}
	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "Content-Type", attrs.ContentType)
	setStrHeader(w, "Content-Language", attrs.ContentLanguage)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attrs.ContentDisposition)
	setIntHeader(w, "Content-Length", objr.Attrs.Size)
	if gzipAcceptable && objr.Attrs.ContentEncoding != "gzip" && isExtensionGzippable(filepath.Ext(object)) {
		// compress content if client supports it and it isn't already compressed
		setStrHeader(w, "ETag", "W/\""+attrs.Etag+"\"") // <- weaken ETag and add quotes to match the ETag HTTP Header format
		logger.Debug("Writing response with gzip", "bucket", bucket, "object", object)
		setStrHeader(w, "Vary", "Accept-Encoding")
		setStrHeader(w, "Content-Encoding", "gzip")
		var buf bytes.Buffer // <-- will hold the compressed response body
		var rw io.WriteCloser

		// check if we need to detect the Content-Type
		if attrs.ContentType == "" {
			// use a special response writer that detects the Content-Type from the first 512 bytes
			// using http.DetectContentType()
			rw = &gzipResponseWriter{
				ResponseWriter: w,
				gzipWriter:     gzip.NewWriter(&buf),
				buf:            &bytes.Buffer{},
			}
		} else {
			// no need to detect the Content-Type, just use a regular gzip writer
			rw = gzip.NewWriter(&buf)
		}

		// copy the GCS response into the gzip writer
		_, err = io.Copy(rw, objr)
		if err != nil {
			logger.Error("Failed to copy response to gzip writer", "error", err, "bucket", bucket, "object", object)
		}

		err = rw.Close()
		if err != nil {
			logger.Error("Failed to close gzip writer", "error", err, "bucket", bucket, "object", object)
		}
		readCloser := io.NopCloser(&buf)
		setIntHeader(w, "Content-Length", int64(buf.Len()))

		// write the compressed response to the client
		_, err = io.Copy(w, readCloser)
		if err != nil {
			logger.Error("Failed to write response", "error", err, "bucket", bucket, "object", object)
		}
	} else {
		if gzipAcceptable {
			setStrHeader(w, "ETag", "W/\""+attrs.Etag+"\"") // <- weaken ETag and add quotes to match the ETag HTTP Header format
		} else {
			setStrHeader(w, "ETag", "\""+attrs.Etag+"\"") // <- add quotes to match the ETag HTTP Header format
		}
		logger.Debug("Writing response without gzip", "bucket", bucket, "object", object)
		_, err = io.Copy(w, objr)
		if err != nil {
			logger.Error("Failed to write response", "error", err, "bucket", bucket, "object", object)
		}
	}
	if err != nil {
		logger.Error("Failed to write response", "error", err, "bucket", bucket, "object", object)
	}
}

func validate(handler func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		params := mux.Vars(r)
		bucket := params["bucket"]
		if bucket == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		object := params["object"]
		if object == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !validateObjectName(object) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		handler(w, r)
	})
}

func healthCheck(w http.ResponseWriter, _ *http.Request) {
	setStrHeader(w, "Content-Type", "text/plain")
	_, err := io.WriteString(w, "OK\n")
	if err != nil {
		logger.Error("Failed to write health check response", "error", err)
	}
}

func main() {
	flag.Parse()

	var err error
	client, err = storage.NewClient(context.Background(), option.WithEndpoint(*gcsApiEndpoint))
	if err != nil {
		logger.Error("Failed to create client", "error", err)
		os.Exit(1)
	}

	r := mux.NewRouter()
	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	r.HandleFunc("/_health", healthCheck).Methods(http.MethodGet, http.MethodHead)
	r.Handle("/_sleep", WithConnectionDraining(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}),
		func() bool { return draining.Load() })).Methods(http.MethodGet)
	r.Handle("/{bucket:[0-9a-zA-Z-_.]+}/{object:.+}", WithConnectionDraining(validate(getOrHead),
		func() bool { return draining.Load() })).Methods(http.MethodGet, http.MethodHead)

	rMetrics := mux.NewRouter()
	rMetrics.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	rMetrics.Handle("/metrics", promhttp.Handler()).Methods(http.MethodGet, http.MethodHead)

	server := &http.Server{
		Addr:        *bind,
		Handler:     r,
		IdleTimeout: *keepAliveTimeout,
		ConnState: func(conn net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				atomic.AddInt64(&openConnectionsCount, 1)
			case http.StateClosed, http.StateHijacked:
				atomic.AddInt64(&openConnectionsCount, -1)
			default:
				// no-op
			}
		},
	}

	h2sMetrics := &http2.Server{
		IdleTimeout: *keepAliveTimeout,
	}
	serverMetrics := &http.Server{
		Addr:        *metricsBind,
		Handler:     h2c.NewHandler(rMetrics, h2sMetrics),
		IdleTimeout: *keepAliveTimeout,
	}

	logger.Info("Listening", "address", *bind)
	logger.Info("Listening for /metrics", "address", *metricsBind)

	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigs
		logger.Info("Received signal.", "signal", sig.String())
		if *preStopSleep > 0 {
			logger.Info("Waiting before stopping the container", "duration", (*preStopSleep).String())
			time.Sleep(*preStopSleep)
		}
		done <- true
	}()

	go func() {
		// start metrics server in the background
		if err := serverMetrics.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serverMetrics.ListenAndServe returned with an error", "error", err)
		}
	}()
	go func() {
		// start main server
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server.ListenAndServe returned with an error", "error", err)
		}
	}()

	// wait for signal to shutdown
	<-done
	logger.Info("Shutting down server now.")
	ctx := context.Background()
	if *serverShutdownTimeout > 0 {
		logger.Info("Waiting until no new connections should come in anymore", "timeout", (*serverShutdownTimeout).String())
		var cancelFunc context.CancelFunc
		ctx, cancelFunc = context.WithTimeout(ctx, *serverShutdownTimeout)
		defer cancelFunc()
	}
	if *drainTimeout > 0 {
		logger.Info("Draining idle connections", "timeout", (*drainTimeout).String())
		draining.Store(true)
		drainStart := time.Now()
		conns := atomic.LoadInt64(&openConnectionsCount)
		for conns > 0 && time.Since(drainStart) < *drainTimeout {
			logger.Info("Waiting for connections to drain.", "connections", conns)
			time.Sleep(1 * time.Second)
			conns = atomic.LoadInt64(&openConnectionsCount)
		}
		logger.Info("Finished draining connections.", "remaining_connections", conns)
	}
	logger.Info("Calling server.Shutdown().")
	err = server.Shutdown(ctx)
	if err != nil {
		logger.Error("server.Shutdown returned with an error", "error", err)
	}
	err = serverMetrics.Shutdown(ctx)
	if err != nil {
		logger.Error("serverMetrics.Shutdown returned with an error", "error", err)
	}
}

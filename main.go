package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	AppName         string
	Env             string
	Host            string
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

type App struct {
	cfg    Config
	logger *log.Logger
	mux    *http.ServeMux
	server *http.Server
}

type healthResponse struct {
	Status    string `json:"status"`
	App       string `json:"app"`
	Env       string `json:"env"`
	Timestamp string `json:"timestamp"`
}

type contextKey string

const requestIDKey contextKey = "requestID"

func main() {
	cfg := loadConfig()
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds|log.LUTC)

	app := NewApp(cfg, logger)
	if err := app.Run(); err != nil {
		logger.Fatalf("application stopped with error: %v", err)
	}
}

func NewApp(cfg Config, logger *log.Logger) *App {
	mux := http.NewServeMux()
	app := &App{
		cfg:    cfg,
		logger: logger,
		mux:    mux,
	}
	app.routes()

	app.server = &http.Server{
		Addr:              joinHostPort(cfg.Host, cfg.Port),
		Handler:           app.withMiddlewares(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return context.Background()
		},
	}

	return app
}

func (a *App) Run() error {
	a.logger.Printf("starting! %s in %s on %s", a.cfg.AppName, a.cfg.Env, a.server.Addr)

	errCh := make(chan error, 1)
	go func() {
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen and serve: %w", err)
			return
		}
		errCh <- nil
	}()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stopCh:
		a.logger.Printf("received signal: %s", sig)
		return a.shutdown()
	case err := <-errCh:
		return err
	}
}

func (a *App) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()

	a.logger.Printf("shutting down with timeout %s", a.cfg.ShutdownTimeout)

	if err := a.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	a.logger.Printf("server stopped")
	return nil
}

func (a *App) routes() {
	a.mux.HandleFunc("/", a.handleIndex)
	a.mux.HandleFunc("/healthz", a.handleHealth)
	a.mux.HandleFunc("/readyz", a.handleReady)
	a.mux.HandleFunc("/api/v1/echo", a.handleEcho)
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "service is running",
		"app":     a.cfg.AppName,
		"env":     a.cfg.Env,
	})
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:    "ok",
		App:       a.cfg.AppName,
		Env:       a.cfg.Env,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *App) handleReady(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ready",
	})
}

func (a *App) handleEcho(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	defer r.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"echo":      body,
		"requestId": requestIDFromContext(r.Context()),
		"now":       time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *App) withMiddlewares(next http.Handler) http.Handler {
	return a.recoverMiddleware(
		a.loggingMiddleware(
			a.requestIDMiddleware(next),
		),
	)
}

func (a *App) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next.ServeHTTP(ww, r)

		a.logger.Printf(
			"request_id=%s method=%s path=%s status=%d duration=%s remote=%s",
			requestIDFromContext(r.Context()),
			r.Method,
			r.URL.Path,
			ww.statusCode,
			time.Since(start),
			r.RemoteAddr,
		)
	})
}

func (a *App) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				a.logger.Printf("panic recovered: %v", rec)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func (a *App) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" {
			requestID = fmt.Sprintf("%d", time.Now().UnixNano())
		}

		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

type statusWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, `{"error":"failed to encode response"}`, http.StatusInternalServerError)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{
		"error": message,
	})
}

func loadConfig() Config {
	return Config{
		AppName:         getEnv("APP_NAME", "my-service"),
		Env:             getEnv("APP_ENV", "local"),
		Host:            getEnv("APP_HOST", "0.0.0.0"),
		Port:            getEnvInt("APP_PORT", 8080),
		ReadTimeout:     getEnvDuration("APP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    getEnvDuration("APP_WRITE_TIMEOUT", 15*time.Second),
		ShutdownTimeout: getEnvDuration("APP_SHUTDOWN_TIMEOUT", 10*time.Second),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	raw := getEnv(key, "")
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	raw := getEnv(key, "")
	if raw == "" {
		return fallback
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}

func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

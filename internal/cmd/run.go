// Package cmd provides command-line interface functionality for the CLI Proxy API server.
// It includes authentication flows for various AI service providers, service startup,
// and other command-line operations.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

// StartService builds and runs the proxy service using the exported SDK.
// It creates a new proxy service instance, sets up signal handling for graceful shutdown,
// and starts the service with the provided configuration.
//
// Parameters:
//   - cfg: The application configuration
//   - configPath: The path to the configuration file
//   - localPassword: Optional password accepted for local management requests
func StartService(cfg *config.Config, configPath string, localPassword string) {
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithLocalManagementPassword(localPassword)

	ctxSignal, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runCtx := ctxSignal
	if localPassword != "" {
		var keepAliveCancel context.CancelFunc
		runCtx, keepAliveCancel = context.WithCancel(ctxSignal)
		builder = builder.WithServerOptions(api.WithKeepAliveEndpoint(10*time.Second, func() {
			log.Warn("keep-alive endpoint idle for 10s, shutting down")
			keepAliveCancel()
		}))
	}

	service, err := builder.Build()
	if err != nil {
		log.Errorf("failed to build proxy service: %v", err)
		return
	}

	err = service.Run(runCtx)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Errorf("proxy service exited with error: %v", err)
	}
}

// StartServiceBackground starts the proxy service in a background goroutine
// and returns a cancel function for shutdown and a done channel.
func StartServiceBackground(cfg *config.Config, configPath string, localPassword string) (cancel func(), done <-chan struct{}) {
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithLocalManagementPassword(localPassword)

	ctx, cancelFn := context.WithCancel(context.Background())
	doneCh := make(chan struct{})

	service, err := builder.Build()
	if err != nil {
		log.Errorf("failed to build proxy service: %v", err)
		close(doneCh)
		return cancelFn, doneCh
	}

	go func() {
		defer close(doneCh)
		if err := service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Errorf("proxy service exited with error: %v", err)
		}
	}()

	return cancelFn, doneCh
}

// WaitForCloudDeploy waits indefinitely for shutdown signals in cloud deploy mode
// when no configuration file is available. It starts a minimal HTTP server to satisfy
// cloud platform health checks and port binding requirements, and accepts config uploads.
func WaitForCloudDeploy() {
	// Clarify that we are intentionally idle for configuration and not running the API server.
	log.Info("Cloud deploy mode: No config found; standing by for configuration. API server is not started. Press Ctrl+C to exit.")

	ctxSignal, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start a minimal HTTP server for health checks and config upload
	port := getCloudDeployPort()
	server := startStandbyServer(port)

	// Block until shutdown signal is received
	<-ctxSignal.Done()
	log.Info("Cloud deploy mode: Shutdown signal received; exiting")

	// Gracefully shutdown the standby server
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warnf("Cloud deploy mode: standby server shutdown error: %v", err)
	}
}

// getCloudDeployPort returns the port to listen on in cloud deploy mode.
// It reads from PORT environment variable (used by Render and other cloud platforms),
// defaulting to 8080 if not set.
func getCloudDeployPort() int {
	if portStr := os.Getenv("PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil && port > 0 {
			return port
		}
	}
	return 8080 // Default port for cloud deploy standby mode
}

// startStandbyServer starts a minimal HTTP server that responds to health checks
// and accepts configuration uploads via Management API.
// This is necessary for cloud platforms like Render that require an open port.
func startStandbyServer(port int) *http.Server {
	mux := http.NewServeMux()
	managementPassword := os.Getenv("MANAGEMENT_PASSWORD")

	// Health check endpoint - returns 200 OK with status message
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"standby","message":"Waiting for configuration. Use Management API to upload config.yaml."}`)
	})

	// Also respond to /health for explicit health checks
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"healthy","mode":"standby"}`)
	})

	// Management API: GET /v0/management/config.yaml - returns current config (empty in standby)
	mux.HandleFunc("/v0/management/config.yaml", func(w http.ResponseWriter, r *http.Request) {
		// Check authorization
		if !authorizeManagement(r, managementPassword) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":"unauthorized","message":"Invalid or missing Authorization header"}`)
			return
		}

		switch r.Method {
		case http.MethodGet:
			// Return empty config or current config if exists
			w.Header().Set("Content-Type", "application/yaml")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "# Configuration file - upload your config.yaml\n")
		case http.MethodPut:
			// Read config content
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":"bad_request","message":"Failed to read request body"}`)
				return
			}

			// Determine config file path
			configPath := getConfigPath()

			// Write config file
			if err := os.WriteFile(configPath, body, 0600); err != nil {
				log.Errorf("Cloud deploy mode: failed to write config file: %v", err)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"error":"internal_error","message":"Failed to write config file"}`)
				return
			}

			log.Infof("Cloud deploy mode: configuration saved to %s, restarting service...", configPath)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"status":"success","message":"Configuration saved. Service will restart."}`)

			// Exit the process - the cloud platform will restart it
			go func() {
				time.Sleep(500 * time.Millisecond)
				os.Exit(0)
			}()
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprintf(w, `{"error":"method_not_allowed","message":"Use GET or PUT"}`)
		}
	})

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		log.Infof("Cloud deploy mode: standby HTTP server listening on port %d", port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warnf("Cloud deploy mode: standby server error: %v", err)
		}
	}()

	return server
}

// authorizeManagement checks if the request has valid management authorization.
func authorizeManagement(r *http.Request, managementPassword string) bool {
	if managementPassword == "" {
		return false
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}

	// Check Bearer token
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		return token == managementPassword
	}

	return false
}

// getConfigPath returns the path where config.yaml should be saved.
func getConfigPath() string {
	// Check for explicit config path
	if configPath := os.Getenv("CONFIG_PATH"); configPath != "" {
		return configPath
	}

	// Default: working directory
	wd, err := os.Getwd()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(wd, "config.yaml")
}

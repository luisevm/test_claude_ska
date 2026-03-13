// FIX #5 — Reference OIDC proxy implementation.
//
// This is a minimal Go-based OIDC discovery proxy that:
//   1. Fetches /.well-known/openid-configuration and /openid/v1/jwks
//      from the local Kubernetes API server using the mounted SA token.
//   2. Rewrites the issuer URL in the discovery document to match the
//      external OIDC URL (ISSUER_URL env var).
//   3. Serves both endpoints on HTTP (behind OpenShift Route TLS edge).
//   4. Refreshes the cached JWKS on a configurable interval.
//   5. Provides /healthz for probes.
//
// Build: go build -o oidc-proxy .
// Container: see Dockerfile in this directory.
//
// Required env vars:
//   ISSUER_URL        — external OIDC issuer URL (set by ACM hub template)
//   REFRESH_INTERVAL  — seconds between JWKS refreshes (default: 300)
//   LISTEN_ADDR       — address to listen on (default: :8080)
package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	issuerURL       string
	listenAddr      string
	refreshInterval time.Duration

	cacheMu         sync.RWMutex
	cachedJWKS      []byte
	cachedDiscovery []byte
	lastRefresh     time.Time
)

func main() {
	issuerURL = os.Getenv("ISSUER_URL")
	if issuerURL == "" {
		log.Fatal("ISSUER_URL environment variable is required")
	}

	listenAddr = os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	intervalSec, _ := strconv.Atoi(os.Getenv("REFRESH_INTERVAL"))
	if intervalSec <= 0 {
		intervalSec = 300
	}
	refreshInterval = time.Duration(intervalSec) * time.Second

	// Initial fetch
	if err := refreshCache(); err != nil {
		log.Printf("WARNING: initial cache refresh failed: %v", err)
	}

	// Background refresh loop
	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := refreshCache(); err != nil {
				log.Printf("ERROR: cache refresh failed: %v", err)
			}
		}
	}()

	http.HandleFunc("/.well-known/openid-configuration", handleDiscovery)
	http.HandleFunc("/openid/v1/jwks", handleJWKS)
	http.HandleFunc("/healthz", handleHealthz)

	log.Printf("Starting OIDC proxy on %s (issuer: %s, refresh: %s)", listenAddr, issuerURL, refreshInterval)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}

func kubeAPIClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				// The in-cluster CA is at /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
				// but for simplicity we skip verification for localhost API calls.
				// In production, load the CA bundle.
				InsecureSkipVerify: true,
			},
		},
	}
}

func readSAToken() (string, error) {
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return "", fmt.Errorf("reading SA token: %w", err)
	}
	return string(token), nil
}

func fetchFromAPI(path string) ([]byte, error) {
	apiHost := os.Getenv("KUBERNETES_SERVICE_HOST")
	apiPort := os.Getenv("KUBERNETES_SERVICE_PORT")
	if apiHost == "" || apiPort == "" {
		return nil, fmt.Errorf("not running in-cluster: KUBERNETES_SERVICE_HOST/PORT not set")
	}

	token, err := readSAToken()
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("https://%s:%s%s", apiHost, apiPort, path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := kubeAPIClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned %d for %s: %s", resp.StatusCode, path, string(body))
	}

	return io.ReadAll(resp.Body)
}

func refreshCache() error {
	// Fetch JWKS
	jwks, err := fetchFromAPI("/openid/v1/jwks")
	if err != nil {
		return fmt.Errorf("fetching JWKS: %w", err)
	}

	// Build discovery document with rewritten issuer URL
	discovery := map[string]interface{}{
		"issuer":                 issuerURL,
		"jwks_uri":              issuerURL + "/openid/v1/jwks",
		"response_types_supported": []string{"id_token"},
		"subject_types_supported":  []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	discoveryJSON, err := json.Marshal(discovery)
	if err != nil {
		return fmt.Errorf("marshaling discovery: %w", err)
	}

	cacheMu.Lock()
	cachedJWKS = jwks
	cachedDiscovery = discoveryJSON
	lastRefresh = time.Now()
	cacheMu.Unlock()

	log.Printf("Cache refreshed successfully (%d bytes JWKS)", len(jwks))
	return nil
}

func handleDiscovery(w http.ResponseWriter, r *http.Request) {
	cacheMu.RLock()
	data := cachedDiscovery
	cacheMu.RUnlock()

	if data == nil {
		http.Error(w, "discovery not yet available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

func handleJWKS(w http.ResponseWriter, r *http.Request) {
	cacheMu.RLock()
	data := cachedJWKS
	cacheMu.RUnlock()

	if data == nil {
		http.Error(w, "JWKS not yet available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	cacheMu.RLock()
	hasData := cachedDiscovery != nil
	age := time.Since(lastRefresh)
	cacheMu.RUnlock()

	if !hasData {
		http.Error(w, "cache empty", http.StatusServiceUnavailable)
		return
	}
	// Unhealthy if cache is older than 2x refresh interval
	if age > 2*refreshInterval {
		http.Error(w, fmt.Sprintf("cache stale: %s old", age), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "ok (cache age: %s)", age.Truncate(time.Second))
}

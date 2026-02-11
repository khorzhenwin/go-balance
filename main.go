package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/khorzhenwin/go-balance/types"
)

// Round robin load balancing to get the next healthy server
func getNextServer(lb *types.LoadBalancer, servers []*types.Server) *types.Server {
	// Lock the load balancer to ensure thread safety
	lb.Mutex.Lock()
	// Unlock the load balancer when done
	defer lb.Mutex.Unlock()

	for i := 0; i < len(servers); i++ {
		// Get the next server index using round robin
		idx := lb.Current % len(servers)
		nextServer := servers[idx]
		// Increment the current index for the next call
		lb.Current++

		// Check if the server is healthy
		nextServer.Mutex.Lock()
		isHealthy := nextServer.IsHealthy
		nextServer.Mutex.Unlock()

		if isHealthy {
			return nextServer
		}
	}
	// No healthy server found
	return nil
}

func probeServerHealth(client *http.Client, s *types.Server) {
	res, err := client.Head(s.URL.String())

	isHealthy := err == nil && res != nil && res.StatusCode == http.StatusOK
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}

	s.Mutex.Lock()
	if !isHealthy {
		fmt.Printf("%s is down\n", s.URL)
	}
	s.IsHealthy = isHealthy
	s.Mutex.Unlock()
}

// Health Check probe by passing in duration for interval -- usually 30 seconds --
func healthCheck(ctx context.Context, client *http.Client, s *types.Server, healthCheckInterval time.Duration) {
	// Probe once immediately so startup state is accurate.
	probeServerHealth(client, s)

	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeServerHealth(client, s)
		}
	}
}

func startDemoBackends(urls []*url.URL) ([]*http.Server, error) {
	var demoServers []*http.Server

	for _, backendURL := range urls {
		listener, err := net.Listen("tcp", backendURL.Host)
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdownServers(shutdownCtx, demoServers)
			return nil, fmt.Errorf("unable to start demo backend on %s: %w", backendURL.Host, err)
		}

		mux := http.NewServeMux()
		serverLabel := backendURL.String()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = fmt.Fprintf(w, "response from %s\n", serverLabel)
			}
		})

		demoServer := &http.Server{Handler: mux}
		demoServers = append(demoServers, demoServer)

		go func(s *http.Server, l net.Listener) {
			if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Demo backend server error on %s: %v", l.Addr().String(), err)
			}
		}(demoServer, listener)
	}

	return demoServers, nil
}

func shutdownServers(ctx context.Context, servers []*http.Server) {
	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Shutdown(ctx); err != nil {
				log.Printf("Server shutdown error: %v", err)
			}
		}(server)
	}
	wg.Wait()
}

// ReverseProxy to forward request to the selected server
func ReverseProxy(s *types.Server) *httputil.ReverseProxy {
	return httputil.NewSingleHostReverseProxy(s.URL)
}

func loadConfig(file string) (types.Config, error) {
	var config types.Config

	data, err := os.ReadFile(file)
	if err != nil {
		return config, err
	}

	err = json.Unmarshal(data, &config)
	if err != nil {
		return config, err
	}

	return config, nil
}

func run() error {
	config, err := loadConfig("config.json")
	if err != nil {
		return fmt.Errorf("error loading configuration: %w", err)
	}

	healthCheckInterval, err := time.ParseDuration(config.HealthCheckInterval)
	if err != nil {
		return fmt.Errorf("invalid health check interval: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var servers []*types.Server
	var backendURLs []*url.URL
	for _, serverUrl := range config.Servers {
		u, err := url.Parse(serverUrl)
		if err != nil {
			return fmt.Errorf("invalid server URL %q: %w", serverUrl, err)
		}
		if u.Host == "" {
			return fmt.Errorf("invalid server URL %q: missing host", serverUrl)
		}

		server := &types.Server{URL: u, IsHealthy: true}
		servers = append(servers, server)
		backendURLs = append(backendURLs, u)
	}

	demoServers, err := startDemoBackends(backendURLs)
	if err != nil {
		return err
	}
	log.Printf("Started %d demo backend servers", len(demoServers))

	healthClient := &http.Client{Timeout: 3 * time.Second}
	for _, server := range servers {
		go healthCheck(ctx, healthClient, server, healthCheckInterval)
	}

	lb := types.LoadBalancer{Current: 0}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		server := getNextServer(&lb, servers)
		if server == nil {
			http.Error(w, "No healthy server available", http.StatusServiceUnavailable)
			return
		}

		// adding this header just for checking from which server the request is being handled.
		// this is not recommended from security perspective as we don't want to let the client know which server is handling the request.
		w.Header().Add("X-Forwarded-Server", server.URL.String())
		ReverseProxy(server).ServeHTTP(w, r)
	})

	lbServer := &http.Server{
		Addr:    config.Port,
		Handler: mux,
	}

	serverErr := make(chan error, 1)
	go func() {
		err := lbServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	log.Println("Starting load balancer on port", config.Port)
	select {
	case <-ctx.Done():
		log.Println("Shutdown signal received, tearing down resources")
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("error starting load balancer: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shutdownServers(shutdownCtx, append([]*http.Server{lbServer}, demoServers...))
	log.Println("All resources shut down cleanly")
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

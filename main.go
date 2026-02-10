package gobalance

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
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

// Health Check probe by passing in duration for interval -- usually 30 seconds --
func healthCheck(s *types.Server, healthCheckInterval time.Duration) {
	for range time.Tick(healthCheckInterval) {
		res, err := http.Head(s.URL.String())
		s.Mutex.Lock()
		if err != nil || res.StatusCode != http.StatusOK {
			fmt.Printf("%s is down\n", s.URL)
			s.IsHealthy = false
		} else {
			s.IsHealthy = true
		}
		s.Mutex.Unlock()
	}
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

func main() {
	config, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("Error loading configuration: %s", err.Error())
	}

	healthCheckInterval, err := time.ParseDuration(config.HealthCheckInterval)
	if err != nil {
		log.Fatalf("Invalid health check interval: %s", err.Error())
	}

	var servers []*types.Server
	for _, serverUrl := range config.Servers {
		u, _ := url.Parse(serverUrl)
		server := &types.Server{URL: u, IsHealthy: true}
		servers = append(servers, server)
		go healthCheck(server, healthCheckInterval)
	}

	lb := types.LoadBalancer{Current: 0}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

	log.Println("Starting load balancer on port", config.Port)
	err = http.ListenAndServe(config.Port, nil)
	if err != nil {
		log.Fatalf("Error starting load balancer: %s\n", err.Error())
	}
}

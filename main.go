package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
)

func main() {
	configFile := flag.String("config", "config.json", "Path to config file")
	flag.Parse()
	loadConfig(*configFile)

	fmt.Printf("A2O Proxy Config Loaded. DebugLevel: %s\n", config.DebugLevel)

	go aggregatorWorker()

	if len(config.Services) == 0 {
		log.Fatal("No services defined in config.")
	}

	var wg sync.WaitGroup
	for i, svc := range config.Services {
		wg.Add(1)
		go func(idx int, s ServiceConfig) {
			defer wg.Done()
			mux := http.NewServeMux()
			handler := makeHandler(func() ServiceConfig { return s }, s.ListenAddress)
			mux.HandleFunc("/v1/messages", handler)
			modelsHandler := makeModelsHandler(func() ServiceConfig { return s })
			mux.HandleFunc("/v1/models", modelsHandler)
			mux.HandleFunc("/models", modelsHandler)
			addCommonEndpoints(mux)
			log.Printf("Starting Service #%d on %s (%s)", idx+1, s.ListenAddress, s.Comment)
			if err := http.ListenAndServe(fixAddr(s.ListenAddress), mux); err != nil {
				log.Printf("[ERR] Service %s failed: %v", s.ListenAddress, err)
			}
		}(i, svc)
	}

	if config.RoundRobinAddress != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mux := http.NewServeMux()
			rrProvider := func() ServiceConfig {
				count := rrCounter.Add(1)
				idx := (count - 1) % uint64(len(config.Services))
				selected := config.Services[idx]
				logDebug("[RR-LB] Selected Service #%d for request", idx+1)
				return selected
			}
			handler := makeHandler(rrProvider, config.RoundRobinAddress)
			mux.HandleFunc("/v1/messages", handler)
			rrModelsHandler := makeModelsHandler(rrProvider)
			mux.HandleFunc("/v1/models", rrModelsHandler)
			mux.HandleFunc("/models", rrModelsHandler)
			addCommonEndpoints(mux)
			log.Printf("Starting Global Round-Robin Listener on %s", config.RoundRobinAddress)
			if err := http.ListenAndServe(fixAddr(config.RoundRobinAddress), mux); err != nil {
				log.Printf("[ERR] Round-Robin Listener %s failed: %v", config.RoundRobinAddress, err)
			}
		}()
	}

	wg.Wait()
}

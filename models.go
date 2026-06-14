package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

func serviceProviderName(svc ServiceConfig) string {
	if name := strings.TrimSpace(svc.Comment); name != "" {
		return name
	}
	return "default"
}

func parseModelMappings(spec string) ([]ModelMapping, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, false
	}

	explicit := false
	var mappings []ModelMapping
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, ":") {
			pair := strings.SplitN(part, ":", 2)
			upstream := strings.TrimSpace(pair[0])
			downstream := strings.TrimSpace(pair[1])
			if upstream == "" || downstream == "" {
				continue
			}
			explicit = true
			mappings = append(mappings, ModelMapping{Upstream: upstream, Downstream: downstream})
			continue
		}
		mappings = append(mappings, ModelMapping{Upstream: part, Downstream: part})
	}
	return mappings, explicit
}

func resolveTargetModel(requestedModel, forceModel string) string {
	forceModel = strings.TrimSpace(forceModel)
	if forceModel == "" {
		return requestedModel
	}

	mappings, explicit := parseModelMappings(forceModel)
	if !explicit {
		return forceModel
	}

	for _, mapping := range mappings {
		if requestedModel == mapping.Downstream || requestedModel == mapping.Upstream {
			return mapping.Upstream
		}
	}
	if len(mappings) == 1 {
		return mappings[0].Upstream
	}
	return requestedModel
}

func modelsEndpoint(openAIBaseURL string) string {
	upstreamURL := strings.TrimSpace(openAIBaseURL)
	if upstreamURL == "" {
		return ""
	}
	if strings.Contains(upstreamURL, "/chat/completions") {
		return strings.Replace(upstreamURL, "/chat/completions", "/models", 1)
	}
	return strings.TrimRight(upstreamURL, "/") + "/models"
}

func modelListPayload(mappings []ModelMapping, svc ServiceConfig) map[string]interface{} {
	data := make([]map[string]interface{}, 0, len(mappings))
	seen := make(map[string]bool)
	provider := serviceProviderName(svc)

	for _, mapping := range mappings {
		id := strings.TrimSpace(mapping.Downstream)
		if id == "" {
			id = strings.TrimSpace(mapping.Upstream)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		data = append(data, map[string]interface{}{
			"id":       id,
			"object":   "model",
			"owned_by": provider,
		})
	}

	return map[string]interface{}{
		"object": "list",
		"data":   data,
	}
}

func writeModelList(w http.ResponseWriter, mappings []ModelMapping, svc ServiceConfig) {
	json.NewEncoder(w).Encode(modelListPayload(mappings, svc))
}

func transformModelsResponse(body []byte, svc ServiceConfig, mappings []ModelMapping, filterMapped bool) ([]byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	rawData, ok := payload["data"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("models response missing data")
	}

	upstreamToDownstream := make(map[string]string, len(mappings))
	for _, mapping := range mappings {
		if mapping.Upstream != "" && mapping.Downstream != "" {
			upstreamToDownstream[mapping.Upstream] = mapping.Downstream
		}
	}

	provider := serviceProviderName(svc)
	seen := make(map[string]bool)
	transformed := make([]interface{}, 0, len(rawData))
	for _, item := range rawData {
		model, ok := item.(map[string]interface{})
		if !ok {
			if !filterMapped {
				transformed = append(transformed, item)
			}
			continue
		}

		id, _ := model["id"].(string)
		if downstream, ok := upstreamToDownstream[id]; ok {
			model["id"] = downstream
		} else if filterMapped {
			continue
		}

		model["owned_by"] = provider
		if exposedID, _ := model["id"].(string); exposedID != "" {
			if seen[exposedID] {
				continue
			}
			seen[exposedID] = true
		}
		transformed = append(transformed, model)
	}

	if filterMapped && len(transformed) == 0 {
		return json.Marshal(modelListPayload(mappings, svc))
	}

	payload["data"] = transformed
	return json.Marshal(payload)
}

func makeModelsHandler(getServiceConfig configProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")

		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		if r.Method != "GET" {
			http.Error(w, "Method Not Allowed", 405)
			return
		}
		if !checkClientAuth(w, r) {
			return
		}

		svc := getServiceConfig()
		mappings, explicitMappings := parseModelMappings(svc.ForceModel)

		// Plain FORCE_MODEL keeps the legacy behavior: expose the forced model directly.
		if svc.ForceModel != "" && !explicitMappings {
			writeModelList(w, []ModelMapping{{Upstream: svc.ForceModel, Downstream: svc.ForceModel}}, svc)
			return
		}

		// Otherwise proxy upstream /models and optionally rewrite upstream ids to downstream ids.
		client := getOrCreateClient(svc)
		upstreamURL := modelsEndpoint(svc.OpenAIBaseURL)
		if upstreamURL == "" {
			if explicitMappings && len(mappings) > 0 {
				writeModelList(w, mappings, svc)
				return
			}
			http.Error(w, "Upstream Not Configured", 502)
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), "GET", upstreamURL, nil)
		if err != nil {
			if explicitMappings && len(mappings) > 0 {
				writeModelList(w, mappings, svc)
				return
			}
			log.Printf("[ERR] Invalid models upstream URL: %v", err)
			http.Error(w, "Bad Upstream URL", 502)
			return
		}
		if svc.OpenAIAPIKey != "" {
			req.Header.Set("Authorization", "Bearer "+svc.OpenAIAPIKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			if explicitMappings && len(mappings) > 0 {
				writeModelList(w, mappings, svc)
				return
			}
			log.Printf("[ERR] Models upstream request failed: %v", err)
			http.Error(w, "Upstream Error", 502)
			return
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			if explicitMappings && len(mappings) > 0 {
				writeModelList(w, mappings, svc)
				return
			}
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}

		body, err = transformModelsResponse(body, svc, mappings, explicitMappings)
		if err != nil {
			if explicitMappings && len(mappings) > 0 {
				writeModelList(w, mappings, svc)
				return
			}
			log.Printf("[WARN] Models response transform failed: %v", err)
			http.Error(w, "Upstream Models Decode Error", 502)
			return
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
	}
}

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func fixAddr(addr string) string {
	if !strings.Contains(addr, ":") {
		return ":" + addr
	}
	return addr
}

func addCommonEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Simple estimation: approximate 1 token per 4 chars
		body, _ := io.ReadAll(r.Body)
		count := len(body) / 4
		if count < 1 {
			count = 1
		}
		json.NewEncoder(w).Encode(map[string]int{"input_tokens": count})
	})
}

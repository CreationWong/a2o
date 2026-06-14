package main

import (
	"log"
	"net/http"
	"strings"
)

func clientAuthToken(r *http.Request) string {
	clientKey := r.Header.Get("x-api-key")
	if clientKey == "" {
		clientKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return clientKey
}

func checkClientAuth(w http.ResponseWriter, r *http.Request) bool {
	if config.AuthToken == "" {
		return true
	}
	if clientAuthToken(r) == config.AuthToken {
		return true
	}
	log.Printf("[AUTH] Failed auth attempt from %s", r.RemoteAddr)
	http.Error(w, "Unauthorized: Invalid API Key", 401)
	return false
}

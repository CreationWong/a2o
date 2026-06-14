package main

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var (
	clientPool   = make(map[string]*http.Client)
	clientPoolMu sync.RWMutex
)

func getOrCreateClient(svc ServiceConfig) *http.Client {
	key := svc.OpenAIBaseURL + "|" + svc.UpstreamProxy

	clientPoolMu.RLock()
	if client, ok := clientPool[key]; ok {
		clientPoolMu.RUnlock()
		return client
	}
	clientPoolMu.RUnlock()

	clientPoolMu.Lock()
	defer clientPoolMu.Unlock()
	if client, ok := clientPool[key]; ok {
		return client
	}

	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Duration(config.TimeoutSeconds) * time.Second,
		ForceAttemptHTTP2:     false,
		DisableCompression:    true,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}

	if svc.UpstreamProxy != "" {
		if proxyUrl, err := url.Parse(svc.UpstreamProxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyUrl)
		} else {
			log.Printf("[WARN] Invalid proxy URL for %s: %v", svc.OpenAIBaseURL, err)
		}
	}

	client := &http.Client{Transport: transport}
	clientPool[key] = client
	log.Printf("[POOL] Created new HTTP client for %s", key)
	return client
}

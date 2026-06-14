package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type configProvider func() ServiceConfig

func makeHandler(getServiceConfig configProvider, listenAddr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		log.Printf("[%s] >>> Request: %s %s from %s", listenAddr, r.Method, r.URL.Path, r.RemoteAddr)
		logDebug("[%s] Headers: %v", listenAddr, r.Header)

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "Method Not Allowed", 405)
			return
		}

		if !checkClientAuth(w, r) {
			return
		}

		svc := getServiceConfig()
		svcName := svc.Comment
		if svcName == "" {
			svcName = svc.ListenAddress
		}

		var antReq AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&antReq); err != nil {
			logDebug("[%s] Bad request body: %v", listenAddr, err)
			http.Error(w, "Bad Request", 400)
			return
		}

		targetModel := resolveTargetModel(antReq.Model, svc.ForceModel)

		logDebug("[%s] REQ Model: %s -> %s, Stream: %v", listenAddr, antReq.Model, targetModel, antReq.Stream)

		oaiReq, err := convertToOpenAI(&antReq, targetModel)
		if err != nil {
			log.Printf("[ERR] Convert failed: %v", err)
			http.Error(w, "Convert Error", 400)
			return
		}

		buf := bufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		json.NewEncoder(buf).Encode(oaiReq)
		oaiBody := buf.Bytes()
		defer bufferPool.Put(buf)

		authKey := svc.OpenAIAPIKey
		if authKey == "" {
			k := r.Header.Get("x-api-key")
			if k == "" {
				k = r.Header.Get("Authorization")
			}
			authKey = strings.TrimPrefix(k, "Bearer ")
		}

		client := getOrCreateClient(svc)
		if strings.TrimSpace(svc.OpenAIBaseURL) == "" {
			http.Error(w, "Upstream Not Configured", 502)
			return
		}

		var resp *http.Response
		var upstreamErr error
		var finalBody io.Reader
		maxRetries := 3

		for i := 0; i < maxRetries; i++ {
			logDebug("[%s] Sending upstream request (attempt %d/%d)", listenAddr, i+1, maxRetries)

			req, err := http.NewRequestWithContext(r.Context(), "POST", svc.OpenAIBaseURL, bytes.NewBuffer(oaiBody))
			if err != nil {
				log.Printf("[ERR] Invalid upstream URL: %v", err)
				http.Error(w, "Bad Upstream URL", 502)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+authKey)

			resp, upstreamErr = client.Do(req)
			if upstreamErr != nil {
				log.Printf("[WARN] Upstream attempt %d/%d failed: %v", i+1, maxRetries, upstreamErr)
				goto RETRY_WAIT
			}

			if resp.StatusCode != 200 {
				finalBody = resp.Body
				break
			}

			if antReq.Stream {
				peekCtx, peekCancel := context.WithCancel(r.Context())
				var peekBuf bytes.Buffer
				peekReader := bufio.NewReader(resp.Body)
				success := false

				done := make(chan bool)
				go func() {
					defer close(done)
					for {
						select {
						case <-peekCtx.Done():
							return
						default:
						}
						line, err := peekReader.ReadBytes('\n')
						if len(line) > 0 {
							peekBuf.Write(line)
						}
						if err != nil {
							return
						}
						if strings.HasPrefix(string(line), "data:") {
							success = true
							return
						}
					}
				}()

				select {
				case <-done:
				case <-time.After(5 * time.Second):
					peekCancel()
					log.Printf("[WARN] Stream Peek Timeout after 5s")
					success = false
				}
				peekCancel()

				if success {
					finalBody = io.MultiReader(&peekBuf, peekReader)
					break
				} else {
					resp.Body.Close()
					upstreamErr = fmt.Errorf("stream peek failed")
				}
			} else {
				finalBody = resp.Body
				break
			}

		RETRY_WAIT:
			if i < maxRetries-1 {
				select {
				case <-r.Context().Done():
					http.Error(w, "Client Disconnected", 499)
					return
				case <-time.After(500 * time.Millisecond):
				}
			}
		}

		if upstreamErr != nil {
			if resp != nil {
				resp.Body.Close()
			}
			log.Printf("[ERR] Upstream Call Failed after %d retries: %v", maxRetries, upstreamErr)
			http.Error(w, "Upstream Error", 502)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(finalBody)
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			log.Printf("[ERR] Upstream replied %s: %s", resp.Status, string(body))
			return
		}

		logCacheInfo(listenAddr, resp.Header, nil)
		if antReq.Stream {
			handleStream(w, finalBody, antReq.Model, svcName, start)
		} else {
			handleNormal(w, finalBody, antReq.Model, svcName, start)
		}
	}
}

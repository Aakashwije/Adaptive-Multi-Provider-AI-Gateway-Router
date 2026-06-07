// Command router is the Adaptive Multi-Provider AI Gateway Router (v0.1).
//
// What it does, end to end:
//
//   - A client POSTs an OpenAI-style chat request to /v1/chat/completions.
//   - The gateway picks a routing MODE (economy / fast / balanced).
//   - The router package scores the configured providers and selects ONE.
//   - The gateway forwards the request to that provider over HTTP.
//   - The provider's answer is returned, wrapped with the routing decision.
//
// The selected provider is one of the local mock backends:
//
//	provider-a -> :9001 (cheaper, slower)
//	provider-b -> :9002 (pricier, faster)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ai-gateway-router/internal/config"
	"ai-gateway-router/internal/providers"
	"ai-gateway-router/internal/router"
)

// ───────────────────────── HTTP request/response contracts ─────────────────────────

// Message is one chat turn (OpenAI-style).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// RoutingOptions carries client-supplied routing hints. For v0.1 that's the mode.
type RoutingOptions struct {
	Mode string `json:"mode"`
}

// ChatRequest is the body accepted by POST /v1/chat/completions.
type ChatRequest struct {
	Messages []Message      `json:"messages"`
	Routing  RoutingOptions `json:"routing"`
}

// ChatResponse is what the gateway returns to the client. It surfaces the
// routing decision (selected_provider + route_reason + trace) alongside the
// downstream provider's actual answer.
type ChatResponse struct {
	SelectedProvider string                  `json:"selected_provider"`
	Mode             string                  `json:"mode"`
	RouteReason      string                  `json:"route_reason"`
	Response         *providers.ChatResponse `json:"response"`
	Trace            []router.CandidateScore `json:"trace"`
}

// errorBody is the uniform error envelope.
type errorBody struct {
	Error string `json:"error"`
}

// ───────────────────────────────── gateway type ─────────────────────────────────

// gateway bundles the dependencies the HTTP handlers need (config + outbound
// client). Using a struct (instead of globals) keeps things testable and tidy.
type gateway struct {
	cfg    *config.Config
	client *providers.Client
}

func main() {
	// Flags (with env fallbacks) so the same binary works in any environment.
	addr := flag.String("addr", envOr("ROUTER_ADDR", ":8080"), "gateway listen address")
	cfgPath := flag.String("config", envOr("ROUTER_CONFIG", "config/router.yaml"), "path to YAML config")
	flag.Parse()

	// Load + validate config. Fail loudly at startup if it's wrong.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	log.Printf("loaded %d provider(s) from %s", len(cfg.Providers), *cfgPath)
	for _, p := range cfg.Providers {
		log.Printf("   %-12s url=%s  cost_score=%.2f  latency=%dms", p.ID, p.URL, p.CostScore, p.LatencyMs)
	}

	gw := &gateway{
		cfg:    cfg,
		client: providers.NewClient(10 * time.Second), // reused for every request
	}

	// Go 1.22+ method-aware routing patterns keep the wiring readable.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", gw.handleChat)
	mux.HandleFunc("GET /healthz", gw.handleHealth)

	srv := &http.Server{
		Addr:         *addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	// Run the server in the background so main can wait for a shutdown signal.
	go func() {
		log.Printf("gateway listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Graceful shutdown on Ctrl-C / SIGTERM: stop accepting new requests and let
	// in-flight ones finish (up to 5s) before exiting.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Println("bye")
}

// ──────────────────────────────────── handlers ────────────────────────────────────

// handleHealth is a trivial liveness probe.
func (g *gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChat is the heart of the gateway: validate -> route -> forward -> respond.
func (g *gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	// 1. Read the body with a hard size cap (defends against huge payloads).
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	// 2. Validate: we need at least one message to do anything useful.
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages must contain at least one item")
		return
	}

	// 3. Decide which provider should handle this request.
	mode := router.NormalizeMode(req.Routing.Mode)
	decision, err := router.Select(g.cfg.Providers, mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Log the decision so you can see routing happen in the terminal.
	log.Print(decision.Reason)

	// 4. Forward the chat to the chosen provider. We pass only the messages
	//    along; the provider context (deadline/cancellation) flows via r.Context().
	forward := map[string]any{"messages": req.Messages}
	provResp, err := g.client.Complete(r.Context(), decision.Provider.URL, forward)
	if err != nil {
		log.Printf("provider %q call failed: %v", decision.Provider.ID, err)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("provider %q failed: %v", decision.Provider.ID, err))
		return
	}
	log.Printf("provider %q responded", decision.Provider.ID)

	// 5. Wrap the provider answer with the routing decision and return it.
	writeJSON(w, http.StatusOK, ChatResponse{
		SelectedProvider: decision.Provider.ID,
		Mode:             decision.Mode,
		RouteReason:      decision.Reason,
		Response:         provResp,
		Trace:            decision.Candidates,
	})
}

// ──────────────────────────────────── helpers ────────────────────────────────────

// writeJSON serialises v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already sent at this point; just log it.
		log.Printf("failed to encode response: %v", err)
	}
}

// writeError is a thin wrapper for returning the uniform error envelope.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// envOr returns the env var value if set, otherwise the fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

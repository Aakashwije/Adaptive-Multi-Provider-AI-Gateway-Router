// Command provider-b is a MOCK AI provider used for local development.
//
// It is intentionally dumb: it ignores the prompt entirely and always returns a
// canned response. Its only job is to give the gateway a real HTTP backend to
// route to, so you can exercise the routing logic without any API keys or cost.
//
// Endpoints:
//
//	POST /v1/chat/completions -> {"provider":"provider-b","message":"..."}
//	GET  /healthz             -> {"status":"ok","provider":"provider-b"}
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
)

// Identity + default port for THIS mock. provider-a is identical but for these.
const (
	providerID  = "provider-b"
	defaultAddr = ":9002"
)

func main() {
	addr := flag.String("addr", defaultAddr, "listen address")
	flag.Parse()

	mux := http.NewServeMux()

	// The "AI" endpoint — always returns the same canned answer.
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"provider": providerID,
			"message":  "Mock AI response from " + providerID,
		})
		log.Printf("[%s] served chat completion", providerID)
	})

	// Liveness probe.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "provider": providerID})
	})

	log.Printf("%s listening on %s", providerID, *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("%s server error: %v", providerID, err)
	}
}

// writeJSON serialises v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

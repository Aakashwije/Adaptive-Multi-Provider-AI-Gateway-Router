// Package router is the routing "brain" of the gateway.
//
// Given the configured providers and a routing MODE, it picks the single best
// provider to serve a request and produces a human-readable reason for the
// choice. It does not perform any I/O — it is pure decision logic, which makes
// it trivial to unit-test.
//
// v0.1 supports three modes:
//
//	economy  -> cheapest provider          (highest cost_score)
//	fast     -> fastest provider           (lowest latency_ms)
//	balanced -> best blend of cost + speed (weighted composite score)
//
// This is a deliberately tiny version of the much larger composite score
// described in the design document (which also factors in live health, quota,
// error rate, cooldowns, etc.). We start small and grow.
package router

import (
	"fmt"
	"sort"
	"strings"

	"ai-gateway-router/internal/config"
)

// Supported routing modes.
const (
	ModeEconomy  = "economy"
	ModeFast     = "fast"
	ModeBalanced = "balanced"
)

// Tunables for BALANCED mode.
//
//	balanced_score = costWeight*cost_score + latencyWeight*latency_score
//
// latency_score normalises raw latency into [0,1] where faster = higher, using a
// "latency budget": a provider at 0ms scores 1.0, and one at/above the budget
// scores 0.0. This is the same normalise-then-weight idea from the design doc.
const (
	costWeight    = 0.5    // how much BALANCED mode cares about price
	latencyWeight = 0.5    // how much BALANCED mode cares about speed
	latencyBudget = 1000.0 // ms; latency at/above this contributes nothing
)

// CandidateScore is the per-provider breakdown returned in the response trace,
// so a caller can SEE exactly how the decision was made (transparency first).
type CandidateScore struct {
	ID           string  `json:"id"`
	CostScore    float64 `json:"cost_score"`
	LatencyMs    int     `json:"latency_ms"`
	LatencyScore float64 `json:"latency_score"`
	Score        float64 `json:"score"` // value actually used to rank in this mode
}

// Decision is the result of routing: the winning provider, the mode used, a
// readable reason, and the full ranked candidate list for debugging.
type Decision struct {
	Provider   config.Provider
	Mode       string
	Reason     string
	Candidates []CandidateScore
}

// NormalizeMode lowercases/trims the mode and defaults empty input to balanced.
func NormalizeMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return ModeBalanced
	}
	return mode
}

// latencyScore converts a raw latency (ms) into a [0,1] score where faster = 1.
func latencyScore(latencyMs int) float64 {
	s := 1.0 - float64(latencyMs)/latencyBudget
	switch {
	case s < 0:
		return 0
	case s > 1:
		return 1
	default:
		return s
	}
}

// Select chooses a provider for the given mode and explains the choice.
//
// In EVERY mode we compute a Score where "higher = better", then sort
// descending. That uniformity is the whole trick: economy uses cost_score as the
// score, fast uses latency_score, and balanced blends both.
func Select(providers []config.Provider, mode string) (*Decision, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers available to route to")
	}
	mode = NormalizeMode(mode)

	// Reject unknown modes up front with a helpful message.
	switch mode {
	case ModeEconomy, ModeFast, ModeBalanced:
		// ok
	default:
		return nil, fmt.Errorf("unknown routing mode %q (valid: economy, fast, balanced)", mode)
	}

	// Score every provider according to the chosen mode.
	cands := make([]CandidateScore, 0, len(providers))
	for _, p := range providers {
		c := CandidateScore{
			ID:           p.ID,
			CostScore:    p.CostScore,
			LatencyMs:    p.LatencyMs,
			LatencyScore: latencyScore(p.LatencyMs),
		}
		switch mode {
		case ModeEconomy:
			// Cheaper is better; cost_score is already "higher = cheaper".
			c.Score = p.CostScore
		case ModeFast:
			// Faster is better; use the normalised latency score.
			c.Score = c.LatencyScore
		case ModeBalanced:
			// Weighted blend of price and speed.
			c.Score = costWeight*p.CostScore + latencyWeight*c.LatencyScore
		}
		cands = append(cands, c)
	}

	// Rank by Score (descending). Ties break on ID for deterministic output.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Score == cands[j].Score {
			return cands[i].ID < cands[j].ID
		}
		return cands[i].Score > cands[j].Score
	})

	winner := cands[0]

	// Look up the winner's full config so the caller knows where to send traffic.
	var chosen config.Provider
	for _, p := range providers {
		if p.ID == winner.ID {
			chosen = p
			break
		}
	}

	return &Decision{
		Provider:   chosen,
		Mode:       mode,
		Reason:     reasonFor(mode, winner),
		Candidates: cands,
	}, nil
}

// reasonFor builds the human-readable explanation that is both logged and
// returned to the client in `route_reason`.
func reasonFor(mode string, w CandidateScore) string {
	switch mode {
	case ModeEconomy:
		return fmt.Sprintf("economy mode -> chose %s: highest cost_score %.2f (cheapest)",
			w.ID, w.CostScore)
	case ModeFast:
		return fmt.Sprintf("fast mode -> chose %s: lowest latency %dms (fastest)",
			w.ID, w.LatencyMs)
	default: // balanced
		return fmt.Sprintf("balanced mode -> chose %s: best blended score %.3f (cost_score %.2f, latency_score %.2f)",
			w.ID, w.Score, w.CostScore, w.LatencyScore)
	}
}

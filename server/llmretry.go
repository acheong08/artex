package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side retry-policy resolution (see the LLM retry design). Of the five layers:
//   - Connection / empty-response / same-provider safety-window settings are
//     endpoint-specific; each LLM profile can override the global default. An
//     unset profile field inherits the global value, or the built-in default if
//     the global value is also unset.
//   - Circuit breaker / intent rerun are process-wide, with one global setting.
//
// Read the global policy from one DB settings row. Call sites are low-frequency
// (provider construction, work wrap-up, configuration saves), so another cache
// is unnecessary. Circuit-breaker settings are an exception: applyRetryPolicy
// pushes them to the Registry for use on every failure path.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// Preserve the raw semantics here: 0 = default, negative = disabled. This
		// matches the SDK's MaxRetries/EmptyResponseRetries and can be resolved there.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// Circuit-breaker (rotation cooldown) defaults match llmpool; only override them
// when explicitly configured. Intent-rerun defaults are modelErrorRetries and
// modelErrorRetryBackoff in engine.go.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s "empty response
// retries" setting because the layers address the same problem in different ways.
// The SDK handles turns with no content blocks by resending the identical request;
// this layer handles reasoning-only turns with no text or tools by adding an
// instruction to continue from existing reasoning (resending the same request is
// ineffective for a no-op caused by context shape). They detect emptiness
// differently because the SDK checks whether any event was yielded, and thinking
// deltas are events. However, "retry empty responses" means retry when the model
// produces no substantive content, so sharing one count matches that expectation.
//
// Read the global policy rather than a profile override: a run may switch profiles
// during failover, but this is the total per-intent quota and should not change
// with the endpoint. Semantics match the SDK's emptyRetries(): 0 = default
// defaultEmptyTurnNudges; negative = disable no-op continuation; positive = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}

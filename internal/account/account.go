// Package account holds the platform account state — the AI gateway a signed-in
// client talks to, the subscription tier it was granted, and the bearer token
// that authenticates it.
//
// It exists because that state used to live in three process environment
// variables (ONECREAT_GATEWAY_URL / _TOKEN / ONECREAT_TIER) that the desktop
// wrote with os.Setenv and that boot, the provider and the slash router each
// read back with os.Getenv. The environment was being used as an application
// state bus: a token refresh "notified" every already-built controller by
// mutating a global, and any component that wanted to know whether the user was
// signed in reached into the process environment to find out.
//
// That is a real problem, not a stylistic one:
//
//   - it is invisible coupling — nothing in a signature says a provider depends
//     on the account state, so nothing stops a second component from writing it;
//   - it is process-global, so two sessions can never hold different accounts,
//     and a test that sets it leaks into every other test in the binary;
//   - it cannot be scoped or handed to a subprocess deliberately, because it is
//     already everywhere.
//
// A Gateway is an explicit object instead. Whoever owns the account (the desktop
// app) holds one and updates it on login, refresh, tier change and logout;
// whoever needs a token takes a CredentialSource. A refresh updates the object
// and the *next* request reads the new token — no os.Setenv, no rebuild.
//
// Env still has one legitimate role, and only one: a **transport**. FromEnv
// imports variables a process was launched with, and Env projects a Gateway back
// out for a subprocess that can only be configured that way. Neither makes the
// environment the source of truth — the object is.
package account

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Environment variable names. They remain the wire format for launching a
// process with an account already attached (and for the dsh adapter boundary
// later), but nothing reads them as live state.
const (
	EnvURL   = "ONECREAT_GATEWAY_URL"
	EnvToken = "ONECREAT_GATEWAY_TOKEN"
	EnvTier  = "ONECREAT_TIER"
)

// CredentialSource yields the bearer token to authenticate the next request.
// Taking one — rather than a string — is what lets a token refresh reach an
// already-running session: the provider asks again on the next call.
type CredentialSource interface {
	Token(ctx context.Context) (string, error)
}

// EnvCredential reads a named environment variable on every call. It backs the
// ordinary bring-your-own-key providers (DEEPSEEK_API_KEY and friends), where
// the environment genuinely *is* the user's configuration rather than runtime
// state the app maintains.
type EnvCredential struct{ Var string }

// Token reads the variable. A missing variable is not an error here: the caller
// (config validation, or the provider's auth-failure path) reports it with far
// better context than this type could.
func (e EnvCredential) Token(context.Context) (string, error) {
	if e.Var == "" {
		return "", nil
	}
	return os.Getenv(e.Var), nil
}

// StaticCredential is a literal key handed in at construction.
type StaticCredential string

// Token returns the key.
func (s StaticCredential) Token(context.Context) (string, error) { return string(s), nil }

// Gateway is the platform account runtime: which AI gateway to call, under which
// tier, with which token. The zero value is a valid signed-out gateway.
//
// It is safe for concurrent use: the desktop's refresh loop writes it while every
// session's provider reads it.
type Gateway struct {
	mu    sync.RWMutex
	url   string
	token string
	tier  string
}

// FromEnv imports a gateway session from the process environment — the
// compatibility path for a process launched with the variables already set.
// Reading them here is *importing* the state, not living in it: from this point
// the returned object is the source of truth, and later mutations of the
// environment are ignored.
func FromEnv() *Gateway {
	return &Gateway{
		url:   strings.TrimSpace(os.Getenv(EnvURL)),
		token: strings.TrimSpace(os.Getenv(EnvToken)),
		tier:  strings.TrimSpace(os.Getenv(EnvTier)),
	}
}

// SetSession attaches a signed-in session. Called on login, on tier change, and
// on every token refresh — the refresh is exactly why this is an object: the
// next request made by any already-running session picks up the new token.
func (g *Gateway) SetSession(url, token, tier string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.url, g.token, g.tier = strings.TrimSpace(url), strings.TrimSpace(token), strings.TrimSpace(tier)
	g.mu.Unlock()
}

// SetToken refreshes just the bearer token, leaving the URL and tier alone.
func (g *Gateway) SetToken(token string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.token = strings.TrimSpace(token)
	g.mu.Unlock()
}

// Clear signs out: the client falls back to whatever provider the local config
// declares.
func (g *Gateway) Clear() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.url, g.token, g.tier = "", "", ""
	g.mu.Unlock()
}

// Active reports whether requests should go through the platform gateway. A nil
// Gateway is inactive, so a caller that was never given one behaves exactly like
// a signed-out client.
func (g *Gateway) Active() bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.url != ""
}

// URL is the gateway's OpenAI-compatible base URL ("" when signed out).
func (g *Gateway) URL() string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.url
}

// Tier is the subscription tier the platform granted ("tier-1/2/3"), which the
// client sends as the model name. The real model behind it is a billing secret.
func (g *Gateway) Tier() string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.tier
}

// Token implements CredentialSource with the current bearer token.
func (g *Gateway) Token(context.Context) (string, error) {
	if g == nil {
		return "", fmt.Errorf("account: no gateway session")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.token, nil
}

// Env projects the gateway back into environment variables, for the one case
// where that is the only channel available: launching a subprocess that must
// inherit the session. It returns "KEY=value" entries suitable for exec.Cmd.Env.
//
// This is a transport, not a state bus — the projection is one-way and taken at
// the moment of launch. A signed-out gateway projects nothing.
func (g *Gateway) Env() []string {
	if !g.Active() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := []string{EnvURL + "=" + g.url, EnvToken + "=" + g.token}
	if g.tier != "" {
		out = append(out, EnvTier+"="+g.tier)
	}
	return out
}

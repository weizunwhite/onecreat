package account

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestRefreshReachesALiveCredentialSource is the whole point of Plan 09 stated as
// a test: a token refreshed after a session was built must reach that session's
// next request. Before, this worked only because the refresh wrote a process
// environment variable that the provider re-read on every call — an invisible
// global that any component could also write.
func TestRefreshReachesALiveCredentialSource(t *testing.T) {
	gw := &Gateway{}
	gw.SetSession("https://gw.example.com/v1", "OLD", "tier-2")

	// A provider captures the source once, at construction.
	var creds CredentialSource = gw

	if tok, _ := creds.Token(context.Background()); tok != "OLD" {
		t.Fatalf("token = %q, want OLD", tok)
	}
	gw.SetToken("NEW")
	if tok, _ := creds.Token(context.Background()); tok != "NEW" {
		t.Fatalf("refreshed token did not reach the already-built source: %q", tok)
	}
	// A refresh must not disturb the rest of the session.
	if gw.URL() != "https://gw.example.com/v1" || gw.Tier() != "tier-2" {
		t.Errorf("refresh changed more than the token: url=%q tier=%q", gw.URL(), gw.Tier())
	}
}

// TestSignedOutGatewayIsInactive: a zero Gateway and a cleared one must both look
// exactly like "no platform account", so a caller that was never given one keeps
// the local-provider behaviour.
func TestSignedOutGatewayIsInactive(t *testing.T) {
	var nilGW *Gateway
	if nilGW.Active() || nilGW.URL() != "" || nilGW.Tier() != "" || nilGW.Env() != nil {
		t.Error("a nil Gateway must read as signed out")
	}
	nilGW.SetSession("x", "y", "z") // must not panic
	nilGW.Clear()

	gw := &Gateway{}
	if gw.Active() {
		t.Error("the zero Gateway must read as signed out")
	}
	gw.SetSession("https://gw.example.com/v1", "tok", "tier-1")
	if !gw.Active() {
		t.Fatal("a session should activate the gateway")
	}
	gw.Clear()
	if gw.Active() || gw.Tier() != "" {
		t.Errorf("Clear should sign out completely: url=%q tier=%q", gw.URL(), gw.Tier())
	}
	if tok, _ := gw.Token(context.Background()); tok != "" {
		t.Errorf("a signed-out gateway should have no token, got %q", tok)
	}
}

// TestFromEnvImportsOnce pins the compatibility contract: the environment is an
// *import*, not a live channel. A process launched with the variables set gets
// them once; mutating them afterwards must not change the object, because that
// is precisely the state bus this package replaced.
func TestFromEnvImportsOnce(t *testing.T) {
	t.Setenv(EnvURL, "https://gw.example.com/v1")
	t.Setenv(EnvToken, "tok")
	t.Setenv(EnvTier, "tier-3")

	gw := FromEnv()
	if !gw.Active() || gw.Tier() != "tier-3" {
		t.Fatalf("import failed: url=%q tier=%q", gw.URL(), gw.Tier())
	}
	t.Setenv(EnvToken, "MUTATED")
	if tok, _ := gw.Token(context.Background()); tok != "tok" {
		t.Errorf("the environment is still being read as live state: token=%q", tok)
	}
}

// TestEnvProjectsForASubprocess: the one legitimate remaining use of the
// variables — handing a session to a process that can only be configured that way.
func TestEnvProjectsForASubprocess(t *testing.T) {
	gw := &Gateway{}
	if gw.Env() != nil {
		t.Error("a signed-out gateway must project nothing")
	}
	gw.SetSession("https://gw.example.com/v1", "tok", "tier-2")
	got := strings.Join(gw.Env(), " ")
	for _, want := range []string{EnvURL + "=https://gw.example.com/v1", EnvToken + "=tok", EnvTier + "=tier-2"} {
		if !strings.Contains(got, want) {
			t.Errorf("Env() = %q, missing %q", got, want)
		}
	}
}

// TestEnvCredentialReadsEachTime: bring-your-own-key providers keep reading their
// variable per request, because there the environment genuinely *is* the user's
// configuration rather than runtime state the app maintains.
func TestEnvCredentialReadsEachTime(t *testing.T) {
	t.Setenv("SOME_VENDOR_KEY", "first")
	c := EnvCredential{Var: "SOME_VENDOR_KEY"}
	if tok, _ := c.Token(context.Background()); tok != "first" {
		t.Fatalf("token = %q", tok)
	}
	t.Setenv("SOME_VENDOR_KEY", "second")
	if tok, _ := c.Token(context.Background()); tok != "second" {
		t.Errorf("an env credential should re-read: %q", tok)
	}
	if tok, err := (EnvCredential{}).Token(context.Background()); err != nil || tok != "" {
		t.Errorf("an unnamed variable should yield (\"\", nil), got (%q, %v)", tok, err)
	}
}

// TestConcurrentRefreshAndRead: the desktop's refresh loop writes while every
// live session's provider reads.
func TestConcurrentRefreshAndRead(t *testing.T) {
	gw := &Gateway{}
	gw.SetSession("https://gw.example.com/v1", "t0", "tier-1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); gw.SetToken("t1") }()
		go func() {
			defer wg.Done()
			_, _ = gw.Token(context.Background())
			_ = gw.Active()
			_ = gw.Tier()
		}()
	}
	wg.Wait()
}

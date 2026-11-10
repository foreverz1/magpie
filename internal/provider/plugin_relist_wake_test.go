package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// kiroListing is the Kiro plugin's provider listing ids.
func kiroListing(ids ...string) []plugin.Provider {
	p := plugin.Provider{ID: "kiro", Spec: "@magpie-community/opencode-kiro-auth", Name: "Kiro", NPM: "@ai-sdk/anthropic"}
	for _, id := range ids {
		p.Models = append(p.Models, plugin.Model{ID: id, Name: id, NPM: "@ai-sdk/anthropic"})
	}
	return []plugin.Provider{p}
}

// relistHarness runs KeepPluginsListed with the plugins' answers given
// by the test, and the wall clock the test's.
type relistHarness struct {
	t       *testing.T
	answers chan []plugin.Provider
	asked   chan struct{}
	mu      sync.Mutex
	now     time.Time
}

func newRelistHarness(t *testing.T, listed []plugin.Provider, picks Provider, soonFor time.Duration) *relistHarness {
	t.Helper()
	claudeHome(t)
	t.Setenv("MAGPIE_BUN", "")
	write := func(name string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(settings.Dir(), name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("plugins.json", map[string]any{"plugins": []map[string]any{{"spec": listed[0].Spec}}})
	write("plugin-auth.json", map[string]any{listed[0].ID: map[string]any{"type": "api", "key": "k"}})
	plugin.UseCached(listed)
	t.Cleanup(func() { plugin.UseCached(nil) })
	if err := Save(picks); err != nil {
		t.Fatal(err)
	}
	h := &relistHarness{t: t, answers: make(chan []plugin.Provider), asked: make(chan struct{}), now: time.Date(2026, 11, 10, 6, 50, 0, 0, time.Local)}
	oldList, oldFirst, oldEvery, oldSoon, oldFor, oldTick, oldClock := relistPlugins, relistFirst, relistEvery, relistSoon, relistSoonFor, relistTick, relistClock
	relistPlugins = func(ctx context.Context) ([]plugin.Provider, error) {
		select {
		case h.asked <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case ps := <-h.answers:
			plugin.UseCached(ps) // as plugin.Providers keeps it
			return ps, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	relistClock = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
	relistFirst, relistTick = time.Millisecond, time.Millisecond
	relistEvery, relistSoon, relistSoonFor = time.Hour, 5*time.Minute, soonFor
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
		relistPlugins, relistFirst, relistEvery, relistSoon, relistSoonFor, relistTick, relistClock = oldList, oldFirst, oldEvery, oldSoon, oldFor, oldTick, oldClock
	})
	go func() { defer close(done); KeepPluginsListed(ctx) }()
	return h
}

func (h *relistHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

// asks waits for the plugins to be asked within the real time given.
func (h *relistHarness) asks(within time.Duration) bool {
	select {
	case <-h.asked:
		return true
	case <-time.After(within):
		return false
	}
}

func (h *relistHarness) picks(id string) (models, delisted []string) {
	h.t.Helper()
	p, err := Find(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return p.Models, p.Delisted
}

// Kiro lists no Claude model while a waking Mac's network isn't back
// (11-07 06:30, 11-10 06:50): the picks claude-opus-5.5 and
// claude-sonnet-5.5 were set aside, and the next list, five minutes on,
// still left them out, so the loop waited its hour, an hour the sleeping
// Mac's timer didn't count. They stayed aside until magpie restarted
// hours later, Claude Code failing with INVALID_MODEL_ID meanwhile. Now a
// pick set aside has its list asked for every relistSoon until it is
// back.
func TestPluginPicksAsideAreAskedForAgainSoon(t *testing.T) {
	full := kiroListing("auto", "claude-opus-5.5", "claude-sonnet-5.5", "glm-5")
	short := kiroListing("auto", "glm-5")
	h := newRelistHarness(t, full, Provider{ID: "kiro", Models: []string{"claude-opus-5.5", "claude-sonnet-5.5"}}, 6*time.Hour)

	if !h.asks(10 * time.Second) {
		t.Fatal("the plugins weren't asked")
	}
	h.answers <- short
	h.advance(5 * time.Minute)
	if !h.asks(10 * time.Second) {
		t.Fatal("a list that left picks out wasn't asked for again soon")
	}
	if got, aside := h.picks("kiro"); len(got) != 0 || !slices.Equal(aside, []string{"claude-opus-5.5", "claude-sonnet-5.5"}) {
		t.Fatalf("short list: picks %v, set aside %v", got, aside)
	}
	// still short: nothing new left, but the picks are still aside
	h.answers <- short
	h.advance(5 * time.Minute)
	if !h.asks(10 * time.Second) {
		t.Fatal("picks still set aside, and the list wasn't asked for again until the hour")
	}
	h.answers <- full
	for deadline := time.Now().Add(10 * time.Second); ; {
		got, aside := h.picks("kiro")
		if slices.Equal(got, []string{"claude-opus-5.5", "claude-sonnet-5.5"}) && len(aside) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("picks didn't come back: %v, set aside %v", got, aside)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// back: the hour again
	h.advance(5 * time.Minute)
	if h.asks(100 * time.Millisecond) {
		t.Fatal("asked again soon with no pick set aside")
	}
}

// The hour between lists is by the wall clock: a Mac asleep through it
// lists again as it wakes, not an hour of waking time later.
func TestPluginsListedAgainByTheWallClock(t *testing.T) {
	full := kiroListing("auto", "claude-opus-5.5")
	h := newRelistHarness(t, full, Provider{ID: "kiro", Models: []string{"claude-opus-5.5"}}, 6*time.Hour)

	if !h.asks(10 * time.Second) {
		t.Fatal("the plugins weren't asked")
	}
	h.answers <- full
	h.advance(30 * time.Minute)
	if h.asks(100 * time.Millisecond) {
		t.Fatal("asked again before the hour")
	}
	h.advance(8 * time.Hour) // the night
	if !h.asks(10 * time.Second) {
		t.Fatal("not asked again on waking")
	}
	h.answers <- full
}

// A vendor that keeps a model out for good doesn't have its list asked
// for every relistSoon for ever: after relistSoonFor, the hour again.
func TestPluginPicksAsideLongAreAskedForHourly(t *testing.T) {
	full := kiroListing("auto", "claude-opus-5.5")
	short := kiroListing("auto")
	h := newRelistHarness(t, full, Provider{ID: "kiro", Models: []string{"auto", "claude-opus-5.5"}}, 30*time.Minute)

	if !h.asks(10 * time.Second) {
		t.Fatal("the plugins weren't asked")
	}
	h.answers <- short
	for range 6 { // asked at 5, 10 … 30 minutes: at 30 the picks have been aside relistSoonFor
		h.advance(5 * time.Minute)
		if !h.asks(10 * time.Second) {
			t.Fatal("not asked again soon within relistSoonFor")
		}
		h.answers <- short
	}
	h.advance(5 * time.Minute)
	if h.asks(100 * time.Millisecond) {
		t.Fatal("still asked every relistSoon past relistSoonFor")
	}
	h.advance(time.Hour)
	if !h.asks(10 * time.Second) {
		t.Fatal("not asked again after the hour")
	}
	h.answers <- short
}

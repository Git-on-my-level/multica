package daemon

import (
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

const (
	accountFactsMaxBytes  = 1 << 20
	accountFactsClockSkew = 5 * time.Minute
)

// agentctl capacity facts v1 is a read-only input. Extra JSON fields are
// allowed so the producer can add metadata independently of the daemon.
type accountFacts struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Accounts      []struct {
		ID      string              `json:"id"`
		Windows []accountFactWindow `json:"windows"`
	} `json:"accounts"`
}

type accountFactWindow struct {
	Name       string    `json:"name"`
	UsedRatio  *float64  `json:"used_ratio"`
	ResetsAt   time.Time `json:"resets_at"`
	ObservedAt time.Time `json:"observed_at"`
}

func readAccountFacts(path string) (accountFacts, bool) {
	var facts accountFacts
	// Refuse special files without blocking on a FIFO at a misconfigured path.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return facts, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > accountFactsMaxBytes {
		return facts, false
	}
	body, err := io.ReadAll(io.LimitReader(file, accountFactsMaxBytes+1))
	if err != nil || len(body) > accountFactsMaxBytes || json.Unmarshal(body, &facts) != nil {
		return facts, false
	}
	return facts, facts.SchemaVersion == 1 && !facts.GeneratedAt.IsZero() && facts.Accounts != nil
}

// loadFacts does file I/O before taking the pool mutex. The legacy policy
// never reads the file.
func (p *accountPool) loadFacts(provider string) map[string][]accountFactWindow {
	if p.policy == cli.AccountPolicySoonestReset {
		return nil
	}
	if p.factsFile == "" {
		p.warnFactsFallback(provider, "not_configured")
		return nil
	}
	facts, ok := readAccountFacts(p.factsFile)
	now := p.now()
	if !ok || facts.GeneratedAt.After(now.Add(accountFactsClockSkew)) {
		p.warnFactsFallback(provider, "unreadable_or_invalid")
		return nil
	}
	if now.Sub(facts.GeneratedAt) > p.factsMaxAge {
		p.warnFactsFallback(provider, "stale")
		return nil
	}
	byID := make(map[string][]accountFactWindow, len(facts.Accounts))
	for _, account := range facts.Accounts {
		_, duplicate := byID[account.ID]
		if strings.TrimSpace(account.ID) == "" || duplicate {
			p.warnFactsFallback(provider, "unreadable_or_invalid")
			return nil
		}
		for i := range account.Windows {
			window := &account.Windows[i]
			if strings.TrimSpace(window.Name) == "" || window.UsedRatio == nil || *window.UsedRatio < 0 {
				p.warnFactsFallback(provider, "unreadable_or_invalid")
				return nil
			}
			if window.ObservedAt.IsZero() {
				window.ObservedAt = facts.GeneratedAt
			}
		}
		byID[account.ID] = account.Windows
	}
	return byID
}

func (p *accountPool) warnFactsFallback(provider, reason string) {
	p.log.Warn("account pool: using run snapshots and soonest_reset", "provider", provider, "reason", reason)
}

// applyCapacityFacts uses facts only when every usable, unexhausted candidate has
// fresh observations. Mixing sources would compare different windows or leave
// part of the pool without projected usage.
func (p *accountPool) applyCapacityFacts(provider string, ranked []accountCandidate, facts map[string][]accountFactWindow, now time.Time) {
	if facts == nil {
		return
	}
	for _, c := range ranked {
		if c.tier == 3 {
			continue
		}
		windows := facts[c.spec.FactsID]
		if len(windows) == 0 {
			p.warnFactsFallback(provider, "account_facts_missing")
			return
		}
		for _, w := range windows {
			if now.Sub(w.ObservedAt) > p.factsMaxAge || w.ObservedAt.After(now.Add(accountFactsClockSkew)) {
				p.warnFactsFallback(provider, "window_observation_out_of_date")
				return
			}
		}
	}
	for i := range ranked {
		c := &ranked[i]
		if c.tier != 3 {
			p.projectAccount(c, facts[c.spec.FactsID], accountKey(provider, c.spec.ID), now)
		}
	}
}

func (p *accountPool) projectAccount(c *accountCandidate, windows []accountFactWindow, key string, now time.Time) {
	c.tier, c.ratio, c.resetsAt = 0, 0, time.Time{}
	c.orderReset = time.Time{}
	active, completed := p.active[key], p.completed[key]
	for _, w := range windows {
		ratio, reset := *w.UsedRatio, w.ResetsAt
		if !reset.IsZero() && !reset.After(now) {
			w.ObservedAt = reset
			ratio, reset = 0, time.Time{}
		}
		observed := sort.Search(len(completed), func(i int) bool { return completed[i].After(w.ObservedAt) })
		pressure := ratio + float64(active+len(completed)-observed+1)*p.expectedRunCost
		if pressure > 1-p.epsilon {
			c.tier = 2
		}
		if ratio > c.ratio || (ratio == c.ratio && reset.After(c.resetsAt)) {
			c.ratio, c.resetsAt = ratio, reset
		}
		if reset.After(c.orderReset) || (reset.Equal(c.orderReset) && pressure > c.orderPressure) {
			c.orderReset, c.orderPressure = reset, pressure
		}
	}
}

// Track this daemon's launches until they finish and each window observation
// accounts for them. Credentials and facts never leave the daemon.
func (p *accountPool) reserveLocked(provider, id string) {
	if p.active != nil {
		p.active[accountKey(provider, id)]++
	}
}

func (t *taskAccount) releaseLocked() {
	p := t.pool
	key := accountKey(t.provider, t.spec.ID)
	if p.active[key] == 0 {
		return
	}
	p.active[key]--
	if t.attempted {
		now := p.now()
		recent := p.completed[key]
		cutoff := now.Add(-p.factsMaxAge)
		old := sort.Search(len(recent), func(i int) bool { return recent[i].After(cutoff) })
		p.completed[key] = append(append(recent[:0], recent[old:]...), now)
		t.attempted = false
	}
}

func (t *taskAccount) release() {
	if t == nil {
		return
	}
	t.pool.mu.Lock()
	defer t.pool.mu.Unlock()
	t.releaseLocked()
}

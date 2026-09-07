// Package file mirrors src/resilience/degrade-pager.ts (FR-GO-07, issue
// #194): the shared one-per-episode degradation pager (PRD §18 Q41 write
// side). Caller-neutral write side: preflight and `devagent
// page-degrade-breach` (the doc-sync surface) both page through it, so the
// threshold, the once-per-episode rule, and the best-effort contract live in
// one place.
//
// Contract, unchanged from the preflight original: fires only on the cycle
// where the trailing streak equals the threshold (mid-streak cycles stay
// silent, the next episode breaches again from zero), and never returns an
// error — a missing webhook, a broken `devagent.json`, a transport throw or
// a non-2xx response must not change the caller's outcome. Paging is
// observability, never a second failure surface on top of the condition it
// reports.
package resilience

import (
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/ledger"
)

// DegradeBreachSource identifies the surface that completed the streak.
type DegradeBreachSource string

const (
	// DegradeBreachSourcePreflight and DegradeBreachSourceDocSync are the
	// two streak surfaces (TS DEGRADE_BREACH_SOURCES const tuple).
	DegradeBreachSourcePreflight DegradeBreachSource = "preflight"
	DegradeBreachSourceDocSync   DegradeBreachSource = "doc-sync"
)

// DegradeBreachSources lists the surfaces that can complete a degradation
// streak and claim the page.
var DegradeBreachSources = []DegradeBreachSource{DegradeBreachSourcePreflight, DegradeBreachSourceDocSync}

// IsDegradeBreachSource reports whether value names a known streak surface.
func IsDegradeBreachSource(value string) bool {
	for _, s := range DegradeBreachSources {
		if string(s) == value {
			return true
		}
	}
	return false
}

// DegradeBreachAlert is the body of the one-per-episode operator paging
// POST (Q41 write side). Field order = TS interface order = JSON key order.
type DegradeBreachAlert struct {
	// Discriminator so a receiver routes the payload without parsing prose.
	Event string `json:"event"` // provider-degraded-breach
	// Which surface recorded the row that completed the streak.
	Source DegradeBreachSource `json:"source"`
	// ISO ts of the paging POST.
	TS string `json:"ts"`
	// Repo whose ledger tripped the streak.
	Repo string `json:"repo"`
	// Role recorded on the breach (loop role; empty when the caller has none).
	Role string `json:"role"`
	// Worker CLI probed ("" when the caller has none).
	Worker string `json:"worker"`
	// Model id probed ("" when the caller has none).
	Model string `json:"model"`
	// Trailing degraded rows; equals Threshold on the breach POST.
	Count int `json:"count"`
	// Threshold the breach check used.
	Threshold int `json:"threshold"`
	// Outage window edges (null when the streak rows carry no parseable ts).
	LatestTs *string `json:"latestTs"`
	// Outage window edges (null when the streak rows carry no parseable ts).
	OldestTs *string `json:"oldestTs"`
	// Outage window width in ms (null when an edge has no parseable ts).
	WindowMs *float64 `json:"windowMs"`
	// Distinct roles degraded in the window, newest first.
	Roles []string `json:"roles"`
	// Bounded human-readable why, carried off the failing surface.
	Detail *string `json:"detail,omitempty"`
}

func (DegradeBreachAlert) operatorAlertEvent() string { return "provider-degraded-breach" }

// DegradeBreachNotifier is the injection seam for the breach transport
// (tests swap it out).
type DegradeBreachNotifier func(url string, alert DegradeBreachAlert) error

// PageDegradeBreachArgs is the caller-supplied context for one paging
// attempt (tests inject Notify).
type PageDegradeBreachArgs struct {
	// Repo owning `devagent.json` and the orchestration ledger.
	RepoPath string
	// Surface that recorded the streak-completing row.
	Source DegradeBreachSource
	// Role recorded on the breach ("" when the caller has none).
	Role string
	// Worker CLI probed ("" when the caller has none).
	Worker string
	// Model id probed ("" when the caller has none).
	Model string
	// Bounded failure excerpt carried onto the alert ("" = omitted).
	Detail string
	// Injection seam for tests: outbound paging transport. Nil defaults to
	// PostOperatorAlert (wrapped for the DegradeBreachAlert shape).
	Notify DegradeBreachNotifier
	// Injectable clock for the alert ts (tests); nil = wall clock.
	Now func() time.Time
}

// PageDegradeBreach pages a human once per outage episode. Call it AFTER the
// degradation row for this cycle lands, so the streak it reads includes this
// cycle. Returns true only when a POST actually went out; every failure mode
// returns false.
func PageDegradeBreach(args PageDegradeBreachArgs) bool {
	cfg, err := config.Load(args.RepoPath)
	if err != nil {
		return false // a broken config file must not turn paging into a caller failure
	}
	url := ""
	if cfg.Resilience != nil {
		url = cfg.Resilience.DegradeWebhookURL
	}
	if url == "" {
		return false
	}
	streak := ReadDegradationStreak(args.RepoPath, DegradeStreakThreshold)
	if streak.Count != streak.Threshold {
		return false
	}
	notify := args.Notify
	if notify == nil {
		notify = func(url string, alert DegradeBreachAlert) error {
			return PostOperatorAlert(url, alert)
		}
	}
	ts := ledger.NowISO()
	if args.Now != nil {
		ts = args.Now().Format("2006-01-02T15:04:05.000Z07:00")
	}
	alert := DegradeBreachAlert{
		Event:     "provider-degraded-breach",
		Source:    args.Source,
		TS:        ts,
		Repo:      args.RepoPath,
		Role:      args.Role,
		Worker:    args.Worker,
		Model:     args.Model,
		Count:     streak.Count,
		Threshold: streak.Threshold,
		LatestTs:  streak.LatestTs,
		OldestTs:  streak.OldestTs,
		WindowMs:  streak.WindowMs,
		Roles:     streak.Roles,
	}
	if args.Detail != "" {
		d := args.Detail
		alert.Detail = &d
	}
	if err := notify(url, alert); err != nil {
		return false // paging is observability, never a failure signal for the loop
	}
	return true
}

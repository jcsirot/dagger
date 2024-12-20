package dagui

import (
	"fmt"
	"math"
	"time"

	"dagger.io/dagger/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/dagger/dagger/dagql/call/callpbv1"
	"github.com/dagger/dagger/engine/slog"
)

type SpanSet = *OrderedSet[SpanID, *Span]

type Span struct {
	SpanSnapshot

	// populated by wireUpSpan
	ParentSpan   *Span          `json:"-"`
	ChildSpans   SpanSet        `json:"-"`
	LinkedFrom   SpanSet        `json:"-"`
	LinksTo      SpanSet        `json:"-"`
	RunningSpans SpanSet        `json:"-"`
	FailedLinks  SpanSet        `json:"-"`
	Call         *callpbv1.Call `json:"-"`
	Base         *callpbv1.Call `json:"-"`

	// NOTE: this is hard coded for Gauge int64 metricdata essentially right now,
	// needs generalization as more metric types get added
	MetricsByName map[string][]metricdata.DataPoint[int64]

	// Indicates that this span was actually exported to the database, and not
	// just allocated due to a span parent or other relationship.
	Received bool

	db *DB
}

// Snapshot returns a snapshot of the span's current state, incrementing its
// Version with every call.
func (span *Span) Snapshot() SpanSnapshot {
	span.Version++
	span.ChildCount = countChildren(span.ChildSpans)
	span.Failed = span.IsFailedOrCausedFailure()
	span.Cached = span.IsCached()
	span.Pending = span.IsPending()
	return span.SpanSnapshot
}

func countChildren(set SpanSet) int {
	count := 0
	for _, child := range set.Order {
		if child.Passthrough {
			count += countChildren(child.ChildSpans)
		} else if !child.Hidden(FrontendOpts{
			// TODO: this should reflect the client side setting
			Verbosity: ShowInternalVerbosity - 1,
		}) {
			count += 1
		}
	}
	return count
}

type SpanSnapshot struct {
	// Monotonically increasing number for each update seen for this span.
	Version int

	ID        SpanID
	Name      string
	StartTime time.Time
	EndTime   time.Time

	Activity Activity `json:",omitempty"`

	ParentID SpanID        `json:",omitempty"`
	Links    []SpanContext `json:",omitempty"`

	Status sdktrace.Status `json:",omitempty"`

	Failed   bool `json:",omitempty"` // includes links/caused failures
	Internal bool `json:",omitempty"`
	Cached   bool `json:",omitempty"`
	Pending  bool `json:",omitempty"`
	Canceled bool `json:",omitempty"`

	Encapsulate  bool `json:",omitempty"`
	Encapsulated bool `json:",omitempty"`
	Mask         bool `json:",omitempty"`
	Passthrough  bool `json:",omitempty"`
	Ignore       bool `json:",omitempty"`

	Inputs []string `json:",omitempty"`
	Output string   `json:",omitempty"`

	EffectID         string   `json:",omitempty"`
	EffectIDs        []string `json:",omitempty"`
	EffectsCompleted []string `json:",omitempty"`

	CallDigest  string `json:",omitempty"`
	CallPayload string `json:",omitempty"`

	ChildCount int  `json:",omitempty"`
	HasLogs    bool `json:",omitempty"`
}

func (snapshot *SpanSnapshot) ProcessAttribute(name string, val any) {
	defer func() {
		// a bit of a shortcut, but there shouldn't be much going on
		// here and all the conversion error handling code is
		// annoying
		if err := recover(); err != nil {
			slog.Warn("panic processing attribute", "name", name, "val", val, "err", err)
		}
	}()

	switch name {
	case telemetry.DagDigestAttr:
		snapshot.CallDigest = val.(string)

	case telemetry.DagCallAttr:
		snapshot.CallPayload = val.(string)

	case telemetry.CachedAttr:
		snapshot.Cached = val.(bool)

	case telemetry.CanceledAttr:
		snapshot.Canceled = val.(bool)

	case telemetry.UIEncapsulateAttr:
		snapshot.Encapsulate = val.(bool)

	case telemetry.UIEncapsulatedAttr:
		snapshot.Encapsulated = val.(bool)

	// encapsulate any gRPC activity by default
	case "rpc.service":
		snapshot.Encapsulated = true

	case telemetry.UIInternalAttr:
		snapshot.Internal = val.(bool)

	case telemetry.UIPassthroughAttr:
		snapshot.Passthrough = val.(bool)

	case telemetry.DagInputsAttr:
		snapshot.Inputs = sliceOf[string](val)

	case telemetry.EffectIDsAttr:
		snapshot.EffectIDs = sliceOf[string](val)

	case telemetry.EffectsCompletedAttr:
		snapshot.EffectsCompleted = sliceOf[string](val)

	case telemetry.DagOutputAttr:
		snapshot.Output = val.(string)

	case telemetry.EffectIDAttr:
		snapshot.EffectID = val.(string)
	}
}

func sliceOf[T any](val any) []T {
	if direct, ok := val.([]T); ok {
		return direct
	}
	slice := val.([]any)
	ts := make([]T, len(slice))
	for i, v := range slice {
		ts[i] = v.(T)
	}
	return ts
}

// PropagateStatusToParentsAndLinks updates the running and failed state of all
// parent spans, linked spans, and their parents to reflect the span.
//
// NOTE: failed state only propagates to spans that installed the current
// span's effect - it does _not_ propagate through the parent span.
func (span *Span) PropagateStatusToParentsAndLinks() {
	for parent := range span.Parents {
		var changed bool
		if span.IsRunningOrLinksRunning() {
			changed = parent.RunningSpans.Add(span)
		} else {
			changed = parent.RunningSpans.Remove(span)
		}
		if changed {
			span.db.updatedSpans.Add(parent)
		}
	}

	for _, linked := range span.LinksTo.Order {
		var changed bool
		if span.IsRunning() {
			changed = linked.RunningSpans.Add(span)
		} else {
			changed = linked.RunningSpans.Remove(span)
		}

		if span.IsFailed() {
			linked.FailedLinks.Add(span)
		}

		if linked.Activity.Add(span) {
			changed = true
		}

		if changed {
			span.db.updatedSpans.Add(linked)
		}

		for parent := range linked.Parents {
			var changed bool
			if span.IsRunning() {
				changed = parent.RunningSpans.Add(span)
			} else {
				changed = parent.RunningSpans.Remove(span)
			}
			if parent.Activity.Add(span) {
				changed = true
			}
			if changed {
				span.db.updatedSpans.Add(parent)
			}
		}
	}
}

func (span *Span) IsOK() bool {
	return span.Status.Code == codes.Ok
}

func (span *Span) IsFailed() bool {
	return span.Status.Code == codes.Error
}

func (span *Span) IsUnset() bool {
	return span.Status.Code == codes.Unset
}

func (span *Span) IsFailedOrCausedFailure() bool {
	if span.Failed {
		// snapshotted, likely based on the following checks
		return true
	}
	if span.Status.Code == codes.Error ||
		len(span.FailedLinks.Order) > 0 {
		return true
	}
	for _, effect := range span.EffectIDs {
		if span.db.FailedEffects[effect] {
			return true
		}
	}
	return false
}

// Errors returns the individual errored spans contributing to the span's
// Failed or CausedFailure status.
func (span *Span) Errors() SpanSet {
	errs := NewSpanSet()
	if span.IsFailed() {
		errs.Add(span)
	}
	if len(errs.Order) > 0 {
		return errs
	}
	for _, failed := range span.FailedLinks.Order {
		errs.Add(failed)
	}
	if len(errs.Order) > 0 {
		return errs
	}
	for _, effect := range span.EffectIDs {
		if span.db.FailedEffects[effect] {
			if effectSpans := span.db.EffectSpans[effect]; effectSpans != nil {
				for _, e := range effectSpans.Order {
					if e.IsFailed() {
						errs.Add(e)
					}
				}
			}
		}
	}
	return errs
}

func (span *Span) FailedReason() (bool, []string) {
	var reasons []string
	if span.Status.Code == codes.Error {
		reasons = append(reasons, "span itself errored")
	}
	for _, failed := range span.FailedLinks.Order {
		reasons = append(reasons, "span has failed link: "+failed.Name)
	}
	for _, effect := range span.EffectIDs {
		if span.db.FailedEffects[effect] {
			reasons = append(reasons, "span installed failed effect: "+effect)
		}
	}
	return len(reasons) > 0, reasons
}

func (span *Span) Parents(f func(*Span) bool) {
	var keepGoing bool
	// if the loop breaks while recursing we need to stop recursing, so we track
	// that by man-in-the-middling the return value.
	recurse := func(s *Span) bool {
		keepGoing = f(s)
		return keepGoing
	}
	if span.ParentSpan != nil {
		if !f(span.ParentSpan) {
			return
		}
		span.ParentSpan.Parents(recurse)
		if !keepGoing {
			return
		}
	}
}

func (span *Span) VisibleParent(opts FrontendOpts) *Span {
	if span.ParentSpan == nil {
		return nil
	}
	// TODO: check links first?
	if span.ParentSpan.Passthrough {
		return span.ParentSpan.VisibleParent(opts)
	}
	links := span.LinksTo.Order
	if len(links) > 0 {
		// prioritize causal spans over the unlazier
		return links[0]
	}
	return span.ParentSpan
}

func (span *Span) Hidden(opts FrontendOpts) bool {
	if span.IsInternal() && opts.Verbosity < ShowInternalVerbosity {
		// internal spans are hidden by default
		return true
	}
	if span.ParentSpan != nil &&
		(span.Encapsulated || span.ParentSpan.Encapsulate) &&
		!span.ParentSpan.IsFailed() &&
		opts.Verbosity < ShowEncapsulatedVerbosity {
		// encapsulated steps are hidden (even on error) unless their parent errors
		return true
	}
	return false
}

func (span *Span) IsRunning() bool {
	return span.EndTime.Before(span.StartTime)
}

func (span *Span) IsRunningOrLinksRunning() bool {
	if span.IsRunning() {
		return true
	}
	for _, link := range span.LinkedFrom.Order {
		if link.IsRunning() {
			return true
		}
	}
	return false
}

func (span *Span) IsPending() bool {
	pending, _ := span.PendingReason()
	return pending
}

func (span *Span) PendingReason() (bool, []string) {
	if span.IsRunningOrLinksRunning() {
		var reasons []string
		if span.IsRunning() {
			reasons = append(reasons, "span is running")
		}
		for _, running := range span.RunningSpans.Order {
			reasons = append(reasons, "span has running link: "+running.Name)
		}
		return false, reasons
	}
	var reasons []string
	if len(span.EffectIDs) > 0 {
		for _, digest := range span.EffectIDs {
			effectSpans := span.db.EffectSpans[digest]
			if effectSpans != nil && len(effectSpans.Order) > 0 {
				return false, []string{
					digest + " has started",
				}
			}
			if span.db.CompletedEffects[digest] {
				return false, []string{
					digest + " has completed",
				}
			}
			reasons = append(reasons, digest+" has not started")
		}
		// there's an output but no linked spans yet, so we're pending
		return true, reasons
	}
	return false, []string{"span has completed"}
}

func (span *Span) IsCached() bool {
	cached, _ := span.CachedReason()
	return cached
}

func (span *Span) CachedReason() (bool, []string) {
	if span.Cached {
		return true, []string{"span is cached"}
	}
	states := map[bool]int{}
	reasons := []string{}
	track := func(effect string, cached bool) {
		states[cached]++
		if cached {
			reasons = append(reasons, fmt.Sprintf("%s is cached", effect))
		} else {
			reasons = append(reasons, fmt.Sprintf("%s is not cached", effect))
		}
	}
	for _, effect := range span.EffectIDs {
		// first check for spans we've seen for the effect
		effectSpans := span.db.EffectSpans[effect]
		if effectSpans != nil && len(effectSpans.Order) > 0 {
			for _, span := range effectSpans.Order {
				track(effect, span.IsCached())
			}
		} else {
			// if the effect is completed but we never saw a span for it, that
			// might mean it was a multiple-layers-deep cache hit. or, some
			// buildkit bug caused us to never see the span. or, another parallel
			// client completed it. in all of those cases, we'll at least consider
			// it cached so it's not stuck 'pending' forever.
			track(effect, span.db.CompletedEffects[effect])
		}
	}
	if len(states) == 1 && states[true] > 0 {
		// all effects were cached
		return true, reasons
	}
	// some effects were not cached
	return false, reasons
}

func (span *Span) HasParent(parent *Span) bool {
	if span.ParentSpan == nil {
		return false
	}
	if span.ParentSpan == parent {
		return true
	}
	return span.ParentSpan.HasParent(parent)
}

// func (step *Step) Inputs() []string {
// 	for _, vtx := range step.db.Intervals[step.Digest] {
// 		return vtx.Inputs // assume all names are equal
// 	}
// 	if step.ID() != nil {
// 		// TODO: in principle this could return arg ID digests, but not needed
// 		return nil
// 	}
// 	return nil
// }

func (span *Span) IsInternal() bool {
	return span.Internal
}

func (span *Span) SelfDuration(fallbackEnd time.Time) time.Duration {
	if span.IsRunningOrLinksRunning() {
		return fallbackEnd.Sub(span.StartTime)
	}
	return span.EndTimeOrFallback(fallbackEnd).Sub(span.StartTime)
}

func (span *Span) EndTimeOrFallback(fallbackEnd time.Time) time.Time {
	if span.IsRunningOrLinksRunning() {
		return fallbackEnd
	}
	maxTime := span.EndTime
	for _, effect := range span.LinkedFrom.Order {
		if effect.EndTime.After(maxTime) {
			maxTime = effect.EndTime
		}
	}
	return maxTime
}

func (span *Span) EndTimeOrNow() time.Time {
	return span.EndTimeOrFallback(time.Now())
}

func (span *Span) Before(other *Span) bool {
	return span.StartTime.Before(other.StartTime)
}

func (span *Span) Classes() []string {
	classes := []string{}
	if span.Cached {
		classes = append(classes, "cached")
	}
	if span.Canceled {
		classes = append(classes, "canceled")
	}
	if span.IsFailed() {
		classes = append(classes, "errored")
	}
	if span.Internal {
		classes = append(classes, "internal")
	}
	return classes
}

func FormatDuration(d time.Duration) string {
	if d < 0 {
		return "INVALID_DURATION"
	}

	days := int64(d.Hours()) / 24
	hours := int64(d.Hours()) % 24
	minutes := int64(d.Minutes()) % 60
	seconds := d.Seconds() - float64(86400*days) - float64(3600*hours) - float64(60*minutes)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", seconds)
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", minutes, int(math.Round(seconds)))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm%ds", hours, minutes, int(math.Round(seconds)))
	default:
		return fmt.Sprintf("%dd%dh%dm%ds", days, hours, minutes, int(math.Round(seconds)))
	}
}

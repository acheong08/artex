package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global settings keys (stored in the settings table; no migration required).
const (
	// settingNotifyEnabled is the global notification switch. Enabled by default;
	// use it as an emergency stop during maintenance, not as the feature's
	// activation condition (the real condition is whether any channel is configured).
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the externally reachable base URL used to create
	// links to finding details (e.g. https://artex.example.com). Empty omits link
	// buttons from messages. No reusable public URL setting existed, so add one here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest interval in minutes.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's poll interval. Three seconds is its
	// freshness bound and the main source of delay between saving a finding and
	// delivering its notification to IM.
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration for claimed deliveries. It must be
	// significantly longer than the maximum send time (the notify HTTP client
	// timeout is 15 seconds), or two dispatchers could send the same row.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick limits events dispatched per cycle, avoiding a burst of
	// delivery jobs for all historical events when a channel is first enabled.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest interval.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick caps deliveries per cycle when a channel has no
	// rate limit. This prevents a single unlimited channel with thousands of
	// findings from blocking one cycle for too long.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the per-cycle send limit for one channel.
	//
	// Derive this limit from the lease duration. Claimed rows receive a lease
	// (notifyLease = 3 minutes); if serial sends in one cycle could exceed it, later
	// leases would expire before delivery. This is harmless in one process (Run is
	// a single goroutine and ticks do not overlap), but with two processes sharing
	// a database, the other process could reclaim expired rows and send them again,
	// double-increment attempts, and mark them failed while the original process
	// is still sending.
	//
	// With a 3-minute lease and 30-second send timeout, 6 sends would consume the
	// entire lease with no margin. Use 5 to cap worst-case send time at 150 seconds,
	// leaving 30 seconds. TestNotifyTickBudgetFitsWithinLease locks this relationship;
	// changing notifyLease, notifySendTimeout, or this value should fail that assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for one delivery. It determines the previous
	// constant; their product must not exceed notifyLease (see TestNotifyTickBudgetFitsWithinLease).
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the retry backoff sequence, indexed by attempts so far.
// Three total attempts (including the first) match db.MaxNotifyAttempts; change both together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the finding-notification delivery engine.
//
// It runs as an independent goroutine alongside Scheduler (see server.New). It
// intentionally has its own tick: notifications have a 3-second freshness target,
// unlike trigger cadence, and failures are isolated so a stuck notification does
// not affect agent triggers.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. There are few channels and little contention, so one
	// mutex is sufficient; finer-grained locking is unnecessary.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is a per-channel token bucket.
//
// Use a token bucket instead of a sliding window that resets a per-minute count.
// Sliding windows have a poor boundary effect: sending 20 at the end of one window
// and another 20 immediately afterward looks like 40 in one second and triggers
// rate limits. Constant-rate replenishment naturally avoids such bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one cycle: dispatch new events, then deliver due jobs.
//
// Failures are logged without stopping the loop; notification failures must never
// become process-level failures. Each tick is independent, and the next cycle retries.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to dispatch event: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to read channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// Token buckets count messages (equivalent to HTTP requests), not findings.
		// They are the same in realtime mode (one message per finding); digest mode
		// combines a batch of findings into one message and consumes one token.
		//
		// In both modes, check the token bucket before claiming jobs; otherwise jobs
		// blocked by rate limiting would already consume retry attempts.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns the token consumption and batch-size limit for a digest channel cycle.
//
// The two return values use different units, which is why they are resolved separately:
//
//   - tokens is the number of messages. A batch of findings becomes one message
//     and one HTTP request, so it is always 1. rate_per_min therefore still applies
//     to digest mode (maximum digest messages per minute).
//   - claimLimit is the maximum number of findings in a batch. It is bounded only
//     by memory and is independent of the request budget.
//
// Previously, request budget per cycle (notifyMaxSendsPerChannelPerTick, derived
// from the lease) was also used as the batch size to apply rate_per_min to digest
// mode. A rate_per_min=20 channel replenishes only one token in a 3-second tick,
// so every digest contained one finding: digest degraded into realtime messages
// with digest wording ("1 finding added in the last 30 minutes"), and
// db.MaxDigestBatchSize was never reached.
//
// End-to-end tests may miss this because they pass a large limit directly to
// stepDigest, bypassing step's budget calculation. Keep the policy here so
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget can assert it directly.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers realtime jobs for one channel, one message per finding.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim real-time deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// Rendering failures are local data problems; retrying will not help.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest combines a channel's pending deliveries into one message when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to check digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Explicitly fail deliveries with invalid snapshots that could not be included
	// in the message. Otherwise they remain outside included, neither in the message
	// nor failure list; after a successful send, bulk state updates would miss them
	// and they would stay in sending until the lease expires and they are reclaimed.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "Event snapshot could not be parsed; this finding cannot be rendered as a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark deliveries with invalid snapshots as failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Pass only entries included in the message to send: included[i] must match
	// msg.Items[i]. send relies on this to map "channel delivered the first K items"
	// to the correct delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers a message and advances delivery state based on the result.
//
// Deliveries in a batch (possibly dozens in digest mode) share one send result:
// either delivered or retry the entire batch. Do not retry individual entries;
// a digest is one message, and partial resend would break batch semantics.
//
// The only exception is segmentation due to channel length limits. If the channel
// reports delivering only the first K items, defer item K+1 onward to the next
// batch instead of marking them successful; otherwise truncated findings would
// be absent from both the message and failure list.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Bound individual sends so a stuck channel does not hold up other channels this cycle.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The channel cannot report more delivered items than were sent. If it
			// does, the renderer is wrong; log and treat all as delivered rather than
			// corrupting records.
			log.Printf("[notify] channel reported %d delivered items, more than the %d queued channel=%s; treating all as delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark deliveries as delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message reached the channel's size limit. Return the rest to the
			// queue for the next tick. Use DeferDeliveries, not RescheduleDeliveries:
			// this is not a failure and should not consume retry budget (the optimistic
			// +1 at claim time is reversed there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("Message reached the channel length limit; only the first %d items were delivered and the rest are queued for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to queue remaining items for follow-up channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel reported neither an error nor how many items it delivered.
		// Treat this as failure (with backoff) so the delivery is not reclaimed
		// forever without ever being marked.
		err = fmt.Errorf("channel did not report the number of delivered items (delivered=%d)", delivered)
	}

	// Decide failure handling per delivery, not by the maximum attempts in the batch.
	//
	// Previously, `if maxAttempts(deliveries) >= MaxNotifyAttempts` failed the
	// entire batch. But entries can have different attempt counts: an older entry
	// retried twice (attempts=2) could drag a new entry (attempts=1) into failed,
	// permanently losing the new finding before it had a retry. This was the
	// opposite of the policy that older rows must not sink newer ones.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] failed to mark deliveries as failed channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("failed after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] failed to mark deliveries as failed channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Group reschedules by delay. There are only three backoff tiers, so the number
	// of groups stays small and avoids one UPDATE per row (which would mean 500
	// round trips for a 500-item batch).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] deliveries failed channel=%d kind=%s permanent=%d retries_exhausted=%d retry_pending=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns entries in all that are not in keep (by pointer identity).
// Used to find deliveries that could not be included in the message; they must be
// explicitly handled and cannot be left in limbo.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt selects the channel implementation and parses its configuration.
// ok=false means the kind is not registered; fail the delivery instead of retrying forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On config-parse failure, pass an empty map so the channel's Validate can
		// report the missing field; that is more useful than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a message for one finding.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders a digest message. Parse snapshots individually so one
// malformed entry is skipped without losing the entire batch.
//
// The returned included entries and msg.Items must correspond exactly (delivery i
// ↔ item i). The caller marks the first K deliveries successful when the channel
// reports delivering the first K items. If a malformed snapshot is skipped here
// but its delivery remains in included, indexes shift: the malformed entry is
// marked delivered and a valid one is treated as undelivered. The caller explicitly
// fails malformed entries; see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A malformed snapshot is excluded from both the message and included;
			// the caller handles it by explicitly marking it failed, not delivered.
			log.Printf("[notify] skipped unparseable snapshot in digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in digest batch could not be parsed", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a delivery item and resolves the asset name and detail link.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Asset-name lookup failure should not block delivery. Missing a name is
		// better than losing the notification; it only removes one line from the message.
		log.Printf("[notify] failed to parse asset name finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// The detail-page route is web/src/app/(main)/function/findings/detail/page.tsx;
		// it reads the finding ID from the id query parameter.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens removes up to want tokens from the channel bucket and returns the actual count.
//
// One token = one message (one HTTP request). In realtime mode the caller passes
// the number of requested messages; in digest mode an entire batch is one message, so pass 1.
//
// Bucket capacity is the channel's per-minute limit, replenished at a constant
// rate. ratePerMin<=0 means unlimited; return a finite but large value so one
// cycle is not held up by an unbounded backlog.
//
// The want cap is essential: without it, the whole bucket would be drained even
// though the caller has its own per-cycle limit. Extra tokens would be unusable
// and disappear before replenishment, making accumulated burst capacity
// unreachable; an empty cycle would still consume tokens.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Replenish according to elapsed wall time at ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before converting to int. Floating-point accumulation can
	// make two half-token refills equal 0.9999999999, which truncates to 0 even
	// though the bucket is mathematically full. 1e-9 is far smaller than one token
	// and cannot cover a meaningful shortfall.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the global switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for deep links, without trailing slashes.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest interval, falling back to the default if invalid or unset.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot for a delivery's event.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("event snapshot for delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse event snapshot for delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// Use the event's type; the snapshot value may have been written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

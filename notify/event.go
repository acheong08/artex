package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. The
// writer is the db-layer finding transaction; readers are the server-layer delivery
// engine and filter matcher. It is defined here as notification-domain data: db only
// serializes it and does not interpret its fields.
//
// Finding fields are stored redundantly rather than looked up during rendering because
// findings can be renamed or have their severity/status changed later. A notification
// must reflect the conclusion **at the time of the event**; looking it up later could
// dangerously imply it was "changed to low afterward." Fan-out and rendering also
// avoid joining the findings/tasks/assets tables.
type Snapshot struct {
	// Event type: finding_created / finding_status_changed.
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Populated only for kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is a finding to be delivered and rendered by a channel.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets contains resolved display names for assets (e.g. domain/IP). The server
	// layer populates it; this package does not access the database.
	Assets []string
	// DetailURL links to the finding details; omitted when public_base_url is unset.
	DetailURL string
	// Used only for status-change events; when both values are present, render as
	// "pending -> fixed."
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether this item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: prefer the manually assigned name, fall
// back to vulnclass, and use a placeholder if both are empty.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(unnamed finding)"
}

// Message contains the complete content for one channel delivery.
type Message struct {
	// Length is 1 for a single delivery and contains the full batch for a digest.
	// An empty slice is invalid; callers must provide at least one item.
	Items []Item
	// When Batch=true, render as a digest (different title, time window, and count).
	Batch bool
	// WindowMinutes is the digest interval in minutes, used in "last N minutes" text
	// only when Batch=true. It is passed explicitly rather than computed with
	// time.Since during rendering, keeping rendering deterministic and testable.
	WindowMinutes int
	// HomeURL is the platform dashboard URL (global public_base_url); omit the link if empty.
	HomeURL string
}

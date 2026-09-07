package integrations

// TicketSpec mirrors src/types.ts TicketSpec. JSON tags preserve the
// TypeScript field names for byte parity.
type TicketSpec struct {
	// ID is the tracker-native identifier, e.g. "LINEAR-204" or "owner/repo#n".
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	Labels             []string `json:"labels"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
	// URL is the raw tracker URL for linking back (omitted when absent,
	// mirroring the TS optional field).
	URL string `json:"url,omitempty"`
	// TrackerInternalID is the tracker-internal object id (e.g. Linear UUID)
	// required for mutations like comments (omitted when absent).
	TrackerInternalID string `json:"trackerInternalId,omitempty"`
}

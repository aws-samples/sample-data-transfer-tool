// Package model holds the data structures shared between the AWS collection
// layer and the UI/render layers. Keeping them here (not in tui) lets the
// non-interactive renderers (table/csv) reuse the exact same shapes.
package model

// ClusterRow is one fully-collected cluster line for the overview table.
// A "-" or empty string in the metric fields means "not available"
// (missing CloudWatch datapoint, ASG not found, etc.) — never fabricate 0.
type ClusterRow struct {
	ClusterID string // full queue-derived id, e.g. gcs-2-s3-worker-c8in-big-pool2
	Short     string // display name (prefix stripped)
	ASG       string // physical ASG name ("" if not resolved)

	Pending  string // ApproximateNumberOfMessages
	InFlight string // ApproximateNumberOfMessagesNotVisible
	DLQ      string // DLQ depth ("-" if no DLQ found)

	RecvPerSec string // NumberOfMessagesReceived / 60
	QPSDone    string // NumberOfMessagesDeleted / 60 (≈ successful objects/sec)

	DownGbps string // ASG NetworkIn  → Gbps ("-" if unavailable)
	UpGbps   string // ASG NetworkOut → Gbps
	CPUPct   string // ASG CPUUtilization avg % ("-" if unavailable)
}

// InstanceProc / RefreshResult live in the aws package (AWS-layer outputs) to
// avoid an import cycle (aws must not import model). The tui/main layers import
// them from aws directly.

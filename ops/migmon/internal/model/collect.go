package model

import (
	"context"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	maws "migmon/internal/aws"
)

// Collector holds the config needed to gather the overview.
type Collector struct {
	Clients    *maws.Clients
	Prefix     string
	ProjectTag string
	Parallel   int // max concurrent clusters collected at once
}

// Collect discovers all clusters and gathers every metric concurrently.
// Returns rows sorted by cluster id for stable display order.
func (c *Collector) Collect(ctx context.Context) ([]ClusterRow, error) {
	clusters, err := c.Clients.Discover(ctx, c.Prefix, c.ProjectTag)
	if err != nil {
		return nil, err
	}
	rows := make([]ClusterRow, len(clusters))

	par := c.Parallel
	if par < 1 {
		par = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(par)

	for i, cl := range clusters {
		i, cl := i, cl
		g.Go(func() error {
			rows[i] = c.collectOne(gctx, cl)
			return nil // per-cluster errors are surfaced as "-"/"?" fields, not fatal
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ClusterID < rows[j].ClusterID })
	return rows, nil
}

func (c *Collector) collectOne(ctx context.Context, cl maws.Cluster) ClusterRow {
	short := strings.TrimPrefix(cl.ClusterID, c.Prefix+"-")
	row := ClusterRow{
		ClusterID: cl.ClusterID,
		Short:     short,
		ASG:       cl.ASG,
		DLQ:       "-",
		DownGbps:  "-", UpGbps: "-", CPUPct: "-",
		RecvPerSec: "0", QPSDone: "0",
		Pending: "?", InFlight: "?",
	}

	// Depth + DLQ (cheap, near-realtime).
	d := c.Clients.Depth(ctx, cl.QueueURL)
	row.Pending, row.InFlight = d.Pending, d.InFlight
	row.DLQ = c.Clients.DLQDepth(ctx, cl.DLQURL)

	// Rates (CloudWatch).
	row.RecvPerSec, row.QPSDone = c.Clients.QueueRates(ctx, cl.QueueName)

	// ASG network + CPU.
	if cl.ASG != "" {
		row.DownGbps = c.Clients.ASGGbps(ctx, cl.ASG, "NetworkIn")
		row.UpGbps = c.Clients.ASGGbps(ctx, cl.ASG, "NetworkOut")
		ids, _ := c.Clients.InServiceInstances(ctx, cl.ASG)
		row.CPUPct = c.Clients.ASGCPUPct(ctx, cl.ASG, ids)
	}
	return row
}

package render

import (
	"fmt"
	"strings"

	"migmon/internal/model"
)

// CSVHeader is the fixed column header line (write once when creating the file).
const CSVHeader = "timestamp,cluster,pending,inflight,dlq,recv_per_s,qps_done,down_gbps,up_gbps,cpu_pct"

// CSV renders one line per cluster, prefixed with the given UTC timestamp.
// header=true prepends CSVHeader. Intended for cron append into a time series.
func CSV(rows []model.ClusterRow, ts string, header bool) string {
	var b strings.Builder
	if header {
		fmt.Fprintln(&b, CSVHeader)
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n",
			ts, r.ClusterID, r.Pending, r.InFlight, r.DLQ,
			r.RecvPerSec, r.QPSDone, r.DownGbps, r.UpGbps, r.CPUPct)
	}
	return b.String()
}

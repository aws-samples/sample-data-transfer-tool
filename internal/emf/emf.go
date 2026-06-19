// Package emf 构造 CloudWatch EMF 文档并输出到 stdout（CW agent 采集）。
// 对齐 Python monitoring_reporter.py：维度白名单 + _aws.Timestamp 必填。
package emf

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws-samples/sample-data-transfer-tool/internal/worker"
)

const namespace = "GcsS3Migration"

// 🔴 维度白名单——唯一允许进入指标维度的键。source 等高基数绝不进维度
// （几十亿基数 → CloudWatch 成本爆炸）。
var allowedDimensions = map[string]bool{
	"QueueType": true, "ErrorClass": true, "InstanceId": true, "State": true, "Op": true,
}

var dimensionNames = []string{"QueueType", "ErrorClass", "InstanceId", "State", "Op"}

// Emit 构造 EMF JSON 并经 out 输出（out 默认 stdout，可注入测试）。
func Emit(ev worker.EMFEvent, out func(string)) error {
	doc, err := build(ev, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	out(string(b))
	return nil
}

func build(ev worker.EMFEvent, tsMillis int64) (map[string]any, error) {
	for _, d := range dimensionNames {
		if !allowedDimensions[d] {
			return nil, fmt.Errorf("高基数维度被守卫拦截: %s", d)
		}
	}
	dims := make([][]string, len(dimensionNames))
	for i, n := range dimensionNames {
		dims[i] = []string{n}
	}
	dims = append(dims, []string{"State", "ErrorClass"})
	fileCount := 0
	if ev.State == "SUCCESS" {
		fileCount = 1
	}
	op := ev.Op
	if op == "" {
		op = "copy"
	}
	return map[string]any{
		"_aws": map[string]any{
			"Timestamp": tsMillis,
			"CloudWatchMetrics": []map[string]any{{
				"Namespace":  namespace,
				"Dimensions": dims,
				"Metrics": []map[string]any{
					{"Name": "TransferredBytes", "Unit": "Bytes"},
					{"Name": "TransferDuration", "Unit": "Seconds"},
					{"Name": "TransferSpeed", "Unit": "Bytes/Second"},
					{"Name": "FileCount", "Unit": "Count"},
					{"Name": "AttemptCount", "Unit": "Count"},
				},
			}},
		},
		"QueueType":        ev.QueueType,
		"ErrorClass":       ev.ErrorClass,
		"InstanceId":       ev.InstanceID,
		"State":            ev.State,
		"Op":               op,
		"TransferredBytes": ev.Bytes,
		"TransferDuration": ev.Elapsed,
		"TransferSpeed":    ev.Speed,
		"FileCount":        fileCount,
		"AttemptCount":     1,
	}, nil
}

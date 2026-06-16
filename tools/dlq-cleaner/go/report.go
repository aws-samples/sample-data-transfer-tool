package main

import (
	"fmt"
	"log"
	"time"
)

// printReport 输出运行结果 + （report 模式下）全量成本/耗时估算。
func printReport(st *stats, cfg cleanerConfig, elapsed time.Duration) {
	scanned := st.scanned
	log.Printf("════════════════ 结果汇总 ════════════════")
	log.Printf("模式        : %s", modeName(cfg.apply))
	log.Printf("已扫描      : %d 条", scanned)
	log.Printf("  源不存在(删): %d", st.deleted)
	log.Printf("  保留(转发)  : %d", st.kept)
	log.Printf("  未知(保守留): %d  （其中无source %d, DDB错误 %d）", st.unknown, st.noSource, st.ddbErrors)
	log.Printf("耗时        : %.1fs", elapsed.Seconds())

	log.Printf("──────────── error_class 分布 ────────────")
	for _, kv := range st.classCounts().sortedByCount() {
		pct := 0.0
		if scanned > 0 {
			pct = float64(kv.Count) * 100 / float64(scanned)
		}
		log.Printf("  %-20s %8d  (%.1f%%)", kv.Class, kv.Count, pct)
	}

	if !cfg.apply && scanned > 0 {
		estimateFull(st, scanned)
	}
}

func modeName(apply bool) string {
	if apply {
		return "CLEAN (--apply，已实际删除/转发)"
	}
	return "REPORT (dry-run，未改动任何数据)"
}

// estimateFull 基于采样比例外推全量（按 1 亿条）成本与耗时，供决策。
func estimateFull(st *stats, sampled int64) {
	const fullScale = 100_000_000.0 // 1 亿假设；用户可据实际 DLQ 深度换算
	delRatio := float64(st.deleted) / float64(sampled)
	keepRatio := float64(st.kept) / float64(sampled)

	// 成本模型（eu-south-2，量级估算）：
	//   DDB Query：每条 1 次，最终一致，按最近 1 行 ~8KB → ~1 RRU；on-demand ~$0.30/百万 RRU
	//   SQS：receive(1亿/10) + delete/send，约 3000 万请求 × $0.40/百万
	ddbCost := fullScale * 1.0 / 1_000_000 * 0.30
	sqsCost := (fullScale/10*2 + fullScale*keepRatio) / 1_000_000 * 0.40
	gcsCost := 0.0 // DDB 方案不碰源端，无 GCS 调用费

	log.Printf("──────────── 全量(按 1 亿条)估算 ────────────")
	log.Printf("  预计删除    : ~%.0f 条 (%.1f%%)", fullScale*delRatio, delRatio*100)
	log.Printf("  预计保留    : ~%.0f 条 (%.1f%%)", fullScale*keepRatio, keepRatio*100)
	log.Printf("  估算费用    : DDB ~$%.0f + SQS ~$%.0f + GCS $%.0f = ~$%.0f",
		ddbCost, sqsCost, gcsCost, ddbCost+sqsCost+gcsCost)
	log.Printf("  估算耗时    : ~%.0f 分钟 (32 worker, on-demand DDB)", fullScale/40000/60)
	fmt.Println()
	log.Printf("提示：实际 DLQ 深度请用 aws sqs get-queue-attributes 查 ApproximateNumberOfMessages，")
	log.Printf("      按真实深度 × 上面比例换算。确认无误后用 mode=clean --apply --archive 实际清理。")
}

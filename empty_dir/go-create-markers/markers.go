package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// folderKey 把 GCS 目录路径转为 S3 _$folder$ 标记 key：末尾 / 去掉后拼 _$folder$。
// 防御性处理极少数不以 / 结尾的行（与 Python 版一致）。
func folderKey(name string) string {
	return strings.TrimSuffix(name, "/") + "_$folder$"
}

// rgKeyOf 生成 row group 的断点 key，绑定输入文件名，避免换输入时串号。
func rgKeyOf(inputPath string, rg int) string {
	return fmt.Sprintf("%s#rg=%d", filepath.Base(inputPath), rg)
}

// job 是流过 jobCh 的一个待创建 marker。
type job struct {
	rg  int
	src string // 源 GCS 目录路径（带末尾 /）
	key string // 目标 S3 key（_$folder$）
}

// result 是 worker 完成一个 put 后发给 logger 的结果。
type result struct {
	rg  int
	src string
	key string
	err error
}

// seal 是 producer 读完一个 RG 后发给 logger 的封口事件：声明该 RG 共产出 emitted 个 job。
type seal struct {
	rg        int
	emitted   int
	truncated bool // 被 --limit 截断：即使全部完成也不可 markDone
}

// rgCounter 由 logger goroutine 独占（无锁）：跟踪单个 RG 的产出/完成/失败计数。
type rgCounter struct {
	emitted   int
	completed int
	failed    int
	sealed    bool
	truncated bool
}

// runConfig 聚合 run 所需参数，避免长参数列表。
type runConfig struct {
	bucket    string
	input     string
	workers   int
	producers int
	limit     int64 // <0 不限
	dryRun    bool
	logPath   string
	failPath  string
	ckptPath  string
}

// run 执行主流程：三段流水线（producer 解码 → jobCh → worker put → 单 logger 写日志+断点）。
// 返回失败计数（>0 时调用方以退出码 2 结束）。
func run(ctx context.Context, cfg *runConfig, s3c s3Putter) (failTotal int64, err error) {
	sr, err := openKeyShard(cfg.input)
	if err != nil {
		return 0, err
	}
	defer sr.close()
	numRG := sr.numRowGroups()
	if numRG == 0 {
		logf("输入无 row group，无事可做")
		return 0, nil
	}

	producers := cfg.producers
	if producers > numRG {
		producers = numRG
	}
	workers := cfg.workers

	ckpt := newCheckpoint(cfg.ckptPath)

	procCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobCh := make(chan job, workers*4)
	resultCh := make(chan result, workers*4)
	sealCh := make(chan seal, numRG)

	var produced int64 // 全局已产出计数（用于 --limit）
	var okTotal, failCnt int64

	// ---- stage 3: 单 logger goroutine（无锁，独占两个 writer 与 RG 计数）----
	var loggerWG sync.WaitGroup
	loggerWG.Add(1)
	go func() {
		defer loggerWG.Done()
		logF, ferr := os.OpenFile(cfg.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if ferr != nil {
			logf("打开日志文件失败 %s: %v", cfg.logPath, ferr)
			return
		}
		defer logF.Close()
		failF, ferr := os.OpenFile(cfg.failPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if ferr != nil {
			logf("打开失败清单文件失败 %s: %v", cfg.failPath, ferr)
			return
		}
		defer failF.Close()

		logW := bufio.NewWriterSize(logF, 1<<20)
		failW := bufio.NewWriterSize(failF, 64<<10)
		defer func() { logW.Flush(); failW.Flush() }()

		counters := make(map[int]*rgCounter, numRG)
		getC := func(rg int) *rgCounter {
			c := counters[rg]
			if c == nil {
				c = &rgCounter{}
				counters[rg] = c
			}
			return c
		}
		// 当一个 RG 既封口又全部完成时结算：无失败且未截断才 markDone。
		settle := func(rg int, c *rgCounter) {
			if !c.sealed || c.completed != c.emitted {
				return
			}
			if c.failed == 0 && !c.truncated {
				ckpt.markDone(rgKeyOf(cfg.input, rg))
			}
			logW.Flush() // RG 边界落盘，对齐断点（崩溃最多丢未flush部分，幂等重做）
			delete(counters, rg)
		}

		flushTick := time.NewTicker(2 * time.Second)
		defer flushTick.Stop()

		resOpen, sealOpen := true, true
		for resOpen || sealOpen {
			select {
			case r, ok := <-resultCh:
				if !ok {
					resOpen = false
					resultCh = nil
					continue
				}
				ts := time.Now().Format("2006-01-02 15:04:05")
				if r.err == nil {
					logW.WriteString(ts + "\tOK\t" + r.src + "\t" + r.key + "\n")
					atomic.AddInt64(&okTotal, 1)
				} else {
					logW.WriteString(ts + "\tFAIL\t" + r.src + "\t" + r.key + "\t" + errorCode(r.err) + "\n")
					failW.WriteString(r.key + "\n")
					failW.Flush() // 失败立即落盘，绝不丢
					atomic.AddInt64(&failCnt, 1)
				}
				c := getC(r.rg)
				c.completed++
				if r.err != nil {
					c.failed++
				}
				settle(r.rg, c)
			case s, ok := <-sealCh:
				if !ok {
					sealOpen = false
					sealCh = nil
					continue
				}
				c := getC(s.rg)
				c.emitted = s.emitted
				c.sealed = true
				c.truncated = s.truncated
				settle(s.rg, c)
			case <-flushTick.C:
				logW.Flush()
			}
		}
	}()

	// ---- stage 2: W put-worker ----
	var workerWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for j := range jobCh {
				var e error
				if !cfg.dryRun {
					e = s3c.put(procCtx, cfg.bucket, j.key)
				}
				resultCh <- result{rg: j.rg, src: j.src, key: j.key, err: e}
			}
		}()
	}

	// ---- stage 1: P producer，按 row group 取模认领 ----
	limitReached := func() bool {
		return cfg.limit >= 0 && atomic.LoadInt64(&produced) >= cfg.limit
	}
	var prodWG sync.WaitGroup
	for p := 0; p < producers; p++ {
		prodWG.Add(1)
		go func(workerID int) {
			defer prodWG.Done()
			for rg := workerID; rg < numRG; rg += producers {
				if procCtx.Err() != nil {
					return
				}
				if ckpt.isDone(rgKeyOf(cfg.input, rg)) {
					sealCh <- seal{rg: rg, emitted: 0} // 已完成 RG：空封口，保持结算一致
					continue
				}
				emitted := 0
				truncated := false
				iterErr := sr.iterRowGroup(rg, func(raw string) error {
					if procCtx.Err() != nil {
						return procCtx.Err()
					}
					if limitReached() {
						truncated = true
						return errLimitReached
					}
					// 原子占额：确保全局不超过 limit
					if cfg.limit >= 0 {
						if atomic.AddInt64(&produced, 1) > cfg.limit {
							atomic.AddInt64(&produced, -1)
							truncated = true
							return errLimitReached
						}
					} else {
						atomic.AddInt64(&produced, 1)
					}
					var key string
					if sr.transform {
						key = folderKey(raw)
					} else {
						key = raw
					}
					emitted++
					select {
					case jobCh <- job{rg: rg, src: raw, key: key}:
					case <-procCtx.Done():
						return procCtx.Err()
					}
					return nil
				})
				sealCh <- seal{rg: rg, emitted: emitted, truncated: truncated}
				if iterErr == errLimitReached {
					return // 本 producer 停止认领更多 RG
				}
				if iterErr != nil && procCtx.Err() == nil {
					logf("读取 row group %d 失败: %v（中止）", rg, iterErr)
					cancel()
					return
				}
			}
		}(p)
	}

	// 进度打印 goroutine（每 5s 一行，对齐 Python 的 batch 进度感）
	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		start := time.Now()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				ok := atomic.LoadInt64(&okTotal)
				fail := atomic.LoadInt64(&failCnt)
				done := ok + fail
				rate := float64(done) / time.Since(start).Seconds()
				logf("进度 成功=%d 失败=%d 速率=%.0f/s", ok, fail, rate)
			}
		}
	}()

	prodWG.Wait()
	close(jobCh)
	close(sealCh)
	workerWG.Wait()
	close(resultCh)
	loggerWG.Wait()
	close(progressDone)

	ok := atomic.LoadInt64(&okTotal)
	fail := atomic.LoadInt64(&failCnt)
	logf("完成。成功=%d 失败=%d 已完成RG=%d/%d", ok, fail, ckpt.count(), numRG)
	logf("逐条明细日志: %s", cfg.logPath)
	if fail > 0 {
		logf("失败 key 已写入: %s（可用 --retry-failed 重跑）", cfg.failPath)
	}
	return fail, nil
}

// dryRunMode 流式打印 src\tkey 映射到 stdout，不写 S3、不动断点。
func dryRunMode(ctx context.Context, cfg *runConfig) error {
	sr, err := openKeyShard(cfg.input)
	if err != nil {
		return err
	}
	defer sr.close()
	w := bufio.NewWriterSize(os.Stdout, 1<<20)
	defer w.Flush()
	fmt.Fprintln(os.Stderr, "--- dry-run（未写 S3）。每行：源(GCS目录) <TAB> 目标(S3 key) ---")
	var n int64
	numRG := sr.numRowGroups()
	for rg := 0; rg < numRG; rg++ {
		if cfg.limit >= 0 && n >= cfg.limit {
			break
		}
		stop := false
		iterErr := sr.iterRowGroup(rg, func(raw string) error {
			if cfg.limit >= 0 && n >= cfg.limit {
				stop = true
				return errLimitReached
			}
			key := raw
			if sr.transform {
				key = folderKey(raw)
			}
			w.WriteString(raw + "\t" + key + "\n")
			n++
			return nil
		})
		if iterErr != nil && iterErr != errLimitReached {
			return iterErr
		}
		if stop {
			break
		}
	}
	w.Flush()
	fmt.Fprintf(os.Stderr, "--- dry-run: 共 %d 条映射（未写 S3）---\n", n)
	return nil
}

// verifyMode 抽样 head_object 核对存在性与 0 字节。
func verifyMode(ctx context.Context, cfg *runConfig, s3c s3Putter, sampleN int) (int, error) {
	sr, err := openKeyShard(cfg.input)
	if err != nil {
		return 1, err
	}
	defer sr.close()

	// 收集足够大的池子（sampleN*50），再均匀抽样，覆盖各处。
	pool := make([]string, 0, sampleN*50)
	numRG := sr.numRowGroups()
	for rg := 0; rg < numRG && len(pool) < cap(pool); rg++ {
		_ = sr.iterRowGroup(rg, func(raw string) error {
			key := raw
			if sr.transform {
				key = folderKey(raw)
			}
			pool = append(pool, key)
			if len(pool) >= cap(pool) {
				return errLimitReached
			}
			return nil
		})
	}
	if len(pool) == 0 {
		logf("输入为空")
		return 1, nil
	}
	step := len(pool) / sampleN
	if step < 1 {
		step = 1
	}
	var okC, badC, missC int
	for i := 0; i < len(pool) && okC+badC+missC < sampleN; i += step {
		res, size, herr := s3c.head(ctx, cfg.bucket, pool[i])
		if herr != nil {
			return 1, fmt.Errorf("head_object 失败 key=%s: %w", pool[i], herr)
		}
		switch res {
		case headOK:
			okC++
		case headBadSize:
			badC++
			logf("[非0字节 %d] %s", size, pool[i])
		case headMissing:
			missC++
			logf("[缺失] %s", pool[i])
		}
	}
	logf("核对 %d 个: 0字节存在=%d 非0字节=%d 缺失=%d", okC+badC+missC, okC, badC, missC)
	if badC == 0 && missC == 0 {
		return 0, nil
	}
	return 1, nil
}

// retryFailedMode 读取 failed_keys.txt 去重重跑，成功的从清单移除。
func retryFailedMode(ctx context.Context, cfg *runConfig, s3c s3Putter) (int, error) {
	f, err := os.Open(cfg.failPath)
	if err != nil {
		if os.IsNotExist(err) {
			logf("无失败文件 %s，无需重跑", cfg.failPath)
			return 0, nil
		}
		return 1, err
	}
	set := make(map[string]struct{})
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			set[line] = struct{}{}
		}
	}
	f.Close()
	if len(set) == 0 {
		logf("失败文件为空，无需重跑")
		return 0, nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	logf("[retry] 重跑 %d 个失败 key …", len(keys))

	jobCh := make(chan string, cfg.workers*4)
	var mu sync.Mutex
	stillFailed := make([]string, 0)
	var wg sync.WaitGroup
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobCh {
				if e := s3c.put(ctx, cfg.bucket, k); e != nil {
					mu.Lock()
					stillFailed = append(stillFailed, k)
					mu.Unlock()
				}
			}
		}()
	}
	for _, k := range keys {
		jobCh <- k
	}
	close(jobCh)
	wg.Wait()

	sort.Strings(stillFailed)
	out, err := os.Create(cfg.failPath)
	if err != nil {
		return 1, err
	}
	w := bufio.NewWriter(out)
	for _, k := range stillFailed {
		w.WriteString(k + "\n")
	}
	w.Flush()
	out.Close()
	logf("[retry] 完成。仍失败=%d", len(stillFailed))
	if len(stillFailed) > 0 {
		return 1, nil
	}
	return 0, nil
}

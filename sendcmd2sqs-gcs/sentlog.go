package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
)

// asyncFileLog 是逐条记录用的异步文件日志器，被发送日志（sentLogger，--log-sent）与各类
// 过滤审计日志（dir/deleted/ignore/size SkipLogger，记录被过滤对象供回查）共用。
//
// 设计要点（应对全量约 2 亿条规模）：
//   - 异步：生产 goroutine 只把字节非阻塞投递进 channel；格式化与写盘在单个后台 goroutine 做，
//     彻底移出热路径（字段提取/转义不拖慢吞吐）。
//   - 不反压：channel 满时丢弃并计数，绝不阻塞生产——日志是辅助，不能拖慢主流程。结束时报告丢弃数。
//   - 大缓冲：4MB bufio 降 syscall 频次。
//
// 包级单例（与 dryRunTracer 同款模式）：本程序是一次性 CLI，单次运行内所有 shard / 两种模式
// 共用同一写入器；nil 表示未启用（dry-run 或 --log-sent=false），各调用点经 nil-safe 的 log() 直接返回。
var (
	sentLogger        *asyncFileLog
	dirSkipLogger     *asyncFileLog
	deletedSkipLogger *asyncFileLog
	ignoreSkipLogger  *asyncFileLog
	sizeSkipLogger    *asyncFileLog
)

// auditLoggers 返回全部审计/发送日志器（含 nil；调用方各方法已 nil-safe）。
// 集中一处，新增日志类型时只改这里。
func auditLoggers() []*asyncFileLog {
	return []*asyncFileLog{sentLogger, dirSkipLogger, deletedSkipLogger, ignoreSkipLogger, sizeSkipLogger}
}

// closeAuditLogs 关闭全部审计/发送日志器（幂等），并返回真正写了文件的本地路径列表，
// 供调用方上传 S3。nil 日志器（未启用）跳过。close 后文件已 flush+gzip 收尾，可安全上传。
func closeAuditLogs() []string {
	var paths []string
	for _, l := range auditLoggers() {
		if l == nil {
			continue
		}
		l.close()
		paths = append(paths, l.localPath)
	}
	return paths
}

// asyncFileLog 见上方包注释。
type asyncFileLog struct {
	ch        chan []byte
	wg        sync.WaitGroup
	w         *bufio.Writer
	gz        *gzip.Writer // 压缩层（写链 bufio→gzip→file）；nil 表示不压缩（测试直写用）
	f         *os.File
	localPath string
	// kind 是日志用途的中文短名（"发送日志"/"目录过滤日志"），用于 close 时的提示与告警。
	kind string
	// writeLine 把一条 payload 格式化写入 w（含行尾 \n）。在后台 goroutine 串行调用，
	// 用静态函数而非闭包：不捕获环境，行格式逻辑一目了然。
	writeLine func(w *bufio.Writer, payload []byte)
	dropped   atomic.Int64 // channel 满时丢弃的条数（日志不完整的信号）
	closeOnce sync.Once    // 保证 close() 幂等（二次 close(ch) 会 panic）
}

// asyncLogChanCap 是投递 channel 容量：足够吸收突发，满了就丢（不反压生产）。
// 设为 50 万：实测单 shard ~93 万条可在 ~9 秒内发完（瞬时峰值达 ~10-20 万条/秒，
// 远超后台单 gzip goroutine 的消化速率），8192 的旧值会丢 ~13%。50 万缓冲可吸收单 shard
// 峰值的绝大部分，shard 间隙后台得以追平。内存代价：满载时约 50万 ×（消息字节~400B + 切片头）
// ≈ 200-250MB 峰值，仅在日志写不过来时才涨到此量，正常很低。
// 注意：这是"吸收突发"而非"无限缓冲"——若发送持续快于消化，极端情况仍可能丢（届时看 dropped 告警）。
const asyncLogChanCap = 500_000

// newAsyncFileLog 创建 {logDir}/{prefix}-{时间戳}.log.gz 并启动后台写盘 goroutine。
// gzip BestSpeed 流式压缩：日志行间高度相似（同目录对象路径），实测压缩 25x+；
// level-1 单 goroutine 吞吐 ~150 万行/秒，远超发送上限（~8 万条/秒），不会成为瓶颈。
// 写链 bufio(4MB)→gzip→file：bufio 在最外层攒批，降低 gzip 小写调用次数。
func newAsyncFileLog(prefix, kind string, writeLine func(*bufio.Writer, []byte)) (*asyncFileLog, error) {
	f, localPath, err := createLogFile(prefix, ".log.gz")
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil { // 仅非法 level 才报错，BestSpeed 不会走到；防御保留
		f.Close()
		return nil, err
	}
	l := &asyncFileLog{
		ch:        make(chan []byte, asyncLogChanCap),
		w:         bufio.NewWriterSize(gz, 4<<20), // 4MB 缓冲
		gz:        gz,
		f:         f,
		localPath: localPath,
		kind:      kind,
		writeLine: writeLine,
	}
	l.wg.Add(1)
	go l.loop()
	return l, nil
}

// newSentLogger 创建发送成功日志（logs/sent-{时间戳}.log，TSV: op\tsource\tdestination）。
func newSentLogger() (*asyncFileLog, error) {
	return newAsyncFileLog("sent", "发送日志", writeSentLine)
}

// newDirSkipLogger 创建目录占位过滤日志（logs/skipped-dirs-{时间戳}.log，每行 bucket/name）。
func newDirSkipLogger() (*asyncFileLog, error) {
	return newAsyncFileLog("skipped-dirs", "目录过滤日志", writeRawLine)
}

// newDeletedSkipLogger 创建已删除对象过滤日志（logs/skipped-deleted-{时间戳}.log，每行 bucket/name）。
// 记录 inventory 中 timeDeleted 非 null 的对象（已不在 GCS，copy 必失败，被拦下）。
func newDeletedSkipLogger() (*asyncFileLog, error) {
	return newAsyncFileLog("skipped-deleted", "已删除对象过滤日志", writeRawLine)
}

// newIgnoreSkipLogger 创建 ignore 规则过滤日志（logs/skipped-ignore-{时间戳}.log，每行 bucket/name）。
// 记录命中 .ignore-gcs glob 规则被跳过的对象，供审计回查。
func newIgnoreSkipLogger() (*asyncFileLog, error) {
	return newAsyncFileLog("skipped-ignore", "ignore过滤日志", writeRawLine)
}

// newSizeSkipLogger 创建 size 区间过滤日志（logs/skipped-size-{时间戳}.log，每行 size\tbucket/name）。
// 记录因 size 超出 [MIN_SIZE, MAX_SIZE] 被跳过的对象（带 size 值便于回查原因）。
func newSizeSkipLogger() (*asyncFileLog, error) {
	return newAsyncFileLog("skipped-size", "size过滤日志", writeSizeLine)
}

// writeSentLine 从消息 JSON 提取 op/source/destination 写 TSV 行。
// 解析放在后台 goroutine（此处），使发送热路径零解析开销。
// 直接 WriteString/WriteByte 而非 fmt.Fprintf：免格式串解析，2 亿条累计省可观时间。
func writeSentLine(w *bufio.Writer, msg []byte) {
	op, source, dest := extractTSV(msg)
	// 对字段值里的 TAB/CR/LF 转义防破坏列结构（复用 escapeTabs）。
	w.WriteString(escapeTabs(op))
	w.WriteByte('\t')
	w.WriteString(escapeTabs(source))
	w.WriteByte('\t')
	w.WriteString(escapeTabs(dest))
	w.WriteByte('\n')
}

// writeRawLine 把 payload 原样写一行（仅转义 TAB/CR/LF，GCS 对象名可含这些字符）。
func writeRawLine(w *bufio.Writer, payload []byte) {
	w.WriteString(escapeTabs(string(payload)))
	w.WriteByte('\n')
}

// writeSizeLine 写 size 过滤日志行：payload 为 "size\tbucket/name"。
// 首个 TAB 是结构性列分隔符（size 是纯数字，无歧义），必须保留为真实 TAB；
// 仅对 bucket/name 段转义（对象名可含 TAB/CR/LF）。不能直接用 writeRawLine——它会把分隔符也转义掉。
func writeSizeLine(w *bufio.Writer, payload []byte) {
	if i := bytes.IndexByte(payload, '\t'); i >= 0 {
		w.Write(payload[:i+1]) // size + 真实 TAB
		w.WriteString(escapeTabs(string(payload[i+1:])))
	} else {
		w.WriteString(escapeTabs(string(payload))) // 防御：无 TAB 时整行转义
	}
	w.WriteByte('\n')
}

// loop 是后台写盘 goroutine：串行消费 channel，逐条格式化写入。
func (l *asyncFileLog) loop() {
	defer l.wg.Done()
	for payload := range l.ch {
		l.writeLine(l.w, payload)
	}
}

// log 投递一条记录（非阻塞）。channel 满则丢弃并计数（不反压生产）。
// 发送日志路径不复制 msg：它由 buildCopyMessage/buildDeleteMessage 独立 json.Marshal 产出，
// sendBatch 全程只读不改写（重试仅截断 entries，不触及 msgs），底层字节稳定可直接投递。
func (l *asyncFileLog) log(payload []byte) {
	if l == nil {
		return
	}
	select {
	case l.ch <- payload:
	default:
		l.dropped.Add(1) // channel 满，丢弃（日志辅助，不阻塞生产）
	}
}

// logPath 投递一条 "bucket/name" 记录（过滤审计日志用）。
// nil 检查先于任何分配——dry-run 时日志器为 nil，热路径上被过滤的行（全量可达亿级）零开销；
// 启用时 make+append 一次成型（对比调用方拼接 string 再转 []byte 的 2 次分配）。
func (l *asyncFileLog) logPath(bucket, name string) {
	if l == nil {
		return
	}
	b := make([]byte, 0, len(bucket)+1+len(name))
	b = append(b, bucket...)
	b = append(b, '/')
	b = append(b, name...)
	l.log(b)
}

// logSize 投递一条 "size\tbucket/name" 记录（size 过滤日志专用，与 writeSizeLine 的解析约定配对：
// 首个 TAB 是结构性列分隔符）。nil 检查先于分配，启用时 AppendInt 入同一缓冲一次成型。
func (l *asyncFileLog) logSize(size int64, bucket, name string) {
	if l == nil {
		return
	}
	b := make([]byte, 0, 20+1+len(bucket)+1+len(name))
	b = strconv.AppendInt(b, size, 10)
	b = append(b, '\t')
	b = append(b, bucket...)
	b = append(b, '/')
	b = append(b, name...)
	l.log(b)
}

// close 关闭 channel、等后台 goroutine 排空、flush、关文件，并报告丢弃数。
// 幂等：用 closeOnce 保护，重复调用安全（收尾上传与 defer 可能都会调）。
func (l *asyncFileLog) close() {
	if l == nil {
		return
	}
	l.closeOnce.Do(func() {
		close(l.ch)
		l.wg.Wait()
		if err := l.w.Flush(); err != nil {
			logf("%s flush 失败 %s: %v", l.kind, l.localPath, err)
		}
		if l.gz != nil {
			if err := l.gz.Close(); err != nil { // 写 gzip footer（缺 footer 的文件 zcat 会报 unexpected EOF）
				logf("%s gzip 收尾失败 %s: %v", l.kind, l.localPath, err)
			}
		}
		if err := l.f.Close(); err != nil {
			logf("关闭%s文件失败 %s: %v", l.kind, l.localPath, err)
		}
		if d := l.dropped.Load(); d > 0 {
			logf("%s已写入 %s（警告：因写入跟不上丢弃 %d 条，日志不完整，但不影响主流程）", l.kind, l.localPath, d)
		} else {
			logf("%s已写入: %s", l.kind, l.localPath)
		}
	})
}

// 三个字段的查找 pattern，包级预构造（避免每条消息每字段重新分配，全量 6 亿次小分配的省除）。
var (
	patOp   = []byte(`"op":"`)
	patSrc  = []byte(`"source":"`)
	patDest = []byte(`"destination":"`)
)

// extractTSV 从一条消息 JSON 中提取 op / source / destination 三个字段的值。
//
// 消息体由 buildCopyMessage / buildDeleteMessage 产出，是 encoding/json 的固定格式输出
// （无空格、字段顺序 source→destination→op→rclone_args）。用轻量字节扫描而非 json.Unmarshal，
// 避免反射开销（全量 2 亿条热路径敏感）。找不到的字段返回空串（防御，正常不会发生）。
func extractTSV(msg []byte) (op, source, dest string) {
	return jsonStringField(msg, patOp), jsonStringField(msg, patSrc), jsonStringField(msg, patDest)
}

// jsonStringField 从固定格式 JSON 字节里取 pat（形如 "key":"）之后的字符串值（值内无转义引号）。
// 对象名（source/destination）来自 GCS/S3 key，不含双引号，故无需处理 JSON 转义。找不到返回 ""。
func jsonStringField(msg, pat []byte) string {
	i := bytes.Index(msg, pat)
	if i < 0 {
		return ""
	}
	start := i + len(pat)
	end := bytes.IndexByte(msg[start:], '"') // 值在下一个 " 处结束（值内无转义引号）
	if end < 0 {
		return ""
	}
	return string(msg[start : start+end])
}

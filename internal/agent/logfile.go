package agent

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"frpeasy/internal/shared"
)

// 客户端日志落盘。
//
// 背景（0.7.26 实测）：Windows 上 agent 由计划任务启动，stdout/stderr 没有人接收，
// 于是「frpc 为什么退出、为什么重启」这类问题在客户端**没有任何证据**——
// 安装目录里只有 agent.state 和二进制本身，面板侧也只有 frps 的日志。
// 一次并发压测里 frpc 进程重启（PID 变了），事后完全无法归因。
//
// 现在 agent 自己写 <data>/agent.log，并把 frpc 的 stdout/stderr 按行转进去
// （`[frpc] ...`），所以「frpc 掉线」至少留下了现场。
const (
	agentLogName = "agent.log"
	// 4MB 足够装下几天的重连日志，再大就滚动：客户端盘上不该长出一个没人管的
	// 大文件。只保留一份 .1，总量上限约 8MB。
	maxLogBytes = 4 << 20
)

// SetupFileLog 把标准 log 输出接到 <dataDir>/agent.log（同时保留 stderr，
// 前台运行时仍能在终端看到）。
//
// 返回的关闭函数在进程退出前调用；不调用也不会丢数据（os.File 不缓冲）。
func SetupFileLog(dataDir string) (func(), error) {
	noop := func() {}
	if strings.TrimSpace(dataDir) == "" {
		return noop, nil
	}
	// 数据目录取不到就不写日志，但不能让 agent 因此起不来：
	// 日志是诊断手段，不是运行前提。
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return noop, err
	}
	path := filepath.Join(dataDir, agentLogName)
	rotateLog(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return noop, err
	}
	log.SetOutput(&logSink{f: f, also: os.Stderr})
	log.SetFlags(log.LstdFlags)
	return func() { _ = f.Close() }, nil
}

// logSink 把日志写进文件，同时尽力复制一份到 stderr。
//
// **不能用 io.MultiWriter**：它的语义是"依次写，遇到第一个错误就返回"，
// 后面的 writer 根本不会被调用。而 Windows 计划任务启动的进程**没有控制台**，
// 往 os.Stderr 写会直接失败 —— 于是 MultiWriter 在第一步就返回错误，
// 真正该写进去的文件一行都没收到。
//
// 这不是理论问题：线上客户端就是这么丢掉整段日志的 —— 进程活了 4 小时、
// 期间反复尝试下载、却只在 agent.log 里留下启动那两行，排查时完全看不到
// "它在干什么、卡在哪"，只能靠翻文件系统里残留的临时文件去猜。
//
// 所以顺序反过来：文件是唯一可信的目的地（写失败才算失败），
// stderr 只是"顺手复制给前台看"，失败就忽略。
type logSink struct {
	mu   sync.Mutex
	f    *os.File
	also io.Writer
}

func (w *logSink) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.also != nil {
		// 忽略错误：没有控制台是常态，不是故障。
		_, _ = w.also.Write(p)
	}
	return w.f.Write(p)
}

// rotateLog 超过上限就滚成 .1（旧的 .1 直接丢掉）。
func rotateLog(path string) {
	st, err := os.Stat(path)
	if err != nil || st.Size() < maxLogBytes {
		return
	}
	_ = os.Remove(path + ".1")
	_ = os.Rename(path, path+".1")
}

// Flush 把缓冲区里剩下的半行写出去。
//
// 必须有这一步：进程崩溃/被 kill 前的最后一行往往没有换行，而那一行
// 恰恰是原因（frpc 的 `start error: ...` 就是这样）。只在 >8KB 时兜底
// 等于把最有价值的一行丢掉。
func (w *lineLogger) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := strings.TrimSpace(shared.StripANSI(strings.TrimRight(string(w.buf), "\r\n"))); s != "" {
		log.Printf("[%s] %s", w.prefix, s)
	}
	w.buf = w.buf[:0]
}

// Write 把子进程按块写出的输出切成行，逐行进 agent 日志。
//
// 直接把 cmd.Stdout 接到 os.Stdout，在服务/计划任务下等于丢进黑洞；
// 而把整块字节当成一行打印，会把 frpc 的多行报错压成一行。
type lineLogger struct {
	prefix string
	mu     sync.Mutex
	buf    []byte
}

func newLineLogger(prefix string) *lineLogger { return &lineLogger{prefix: prefix} }

func (w *lineLogger) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := -1
		for k, c := range w.buf {
			if c == '\n' {
				i = k
				break
			}
		}
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		// frpc 的每行都带 SGR（`ESC[0m ESC[1;34m`）：不去掉的话日志文件里
		// 每行前面挂一串 `[0m[1;34m`，而且「只有转义、没有正文」的空行会
		// 变成一行光秃秃的 `[frpc] `（0.7.27 首次上线实测到的）。
		line = strings.TrimSpace(shared.StripANSI(line))
		if line != "" {
			log.Printf("[%s] %s", w.prefix, line)
		}
		w.buf = w.buf[i+1:]
	}
	// frpc 偶尔会输出不换行的进度/报错巨块：超过 8KB 直接倒出来，
	// 否则这段内容会一直压着不落盘，出事时正好缺这一段。
	if len(w.buf) > 8192 {
		if s := strings.TrimSpace(shared.StripANSI(string(w.buf))); s != "" {
			log.Printf("[%s] %s", w.prefix, s)
		}
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// maxUploadLogBytes 回传给面板的日志上限。
//
// 64KB ≈ 最近 400~600 行，够看清「为什么掉线」；再大就要考虑上传时间与
// 面板库体积，而尾部之后的内容对排障没有价值。
const maxUploadLogBytes = 64 << 10

// TailLog 读日志文件尾部（最多 maxBytes），并保证从行边界开始。
//
// 必须按行边界截：半个时间戳开头的第一行会被当成"日志坏了"，
// 也会让用户以为时间对不上（与面板 frps 日志同一个坑）。
func TailLog(path string, maxBytes int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	size := st.Size()
	start := int64(0)
	if size > int64(maxBytes) {
		start = size - int64(maxBytes)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	// 起点落在行中间时，丢掉第一段半行。
	if start > 0 {
		head := b
		if len(head) > 512 {
			head = head[:512]
		}
		if i := indexByte(head, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return shared.StripANSI(string(b))
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

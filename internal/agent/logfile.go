package agent

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.SetFlags(log.LstdFlags)
	return func() { _ = f.Close() }, nil
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
	if s := strings.TrimRight(string(w.buf), "\r\n"); strings.TrimSpace(s) != "" {
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
		if strings.TrimSpace(line) != "" {
			log.Printf("[%s] %s", w.prefix, line)
		}
		w.buf = w.buf[i+1:]
	}
	// frpc 偶尔会输出不换行的进度/报错巨块：超过 8KB 直接倒出来，
	// 否则这段内容会一直压着不落盘，出事时正好缺这一段。
	if len(w.buf) > 8192 {
		log.Printf("[%s] %s", w.prefix, string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

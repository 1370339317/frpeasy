//go:build windows

package agent

import (
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 为什么 Windows 需要 Job Object：
//
// Windows 没有 cgroup，agent 被强杀（任务管理器、Stop-Process -Force、系统更新
// 强制重启）时收不到任何通知，它拉起的 frpc 不会跟着退出，变成一个没人管的
// 孤儿进程：隧道在面板上照样显示 online（frps 那头确实还连着），客户端却显示
// offline —— 两边状态互相打架，本机端口也一直被占着，只有手工杀进程才能恢复。
//
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 让内核在 agent 进程消失（最后一个句柄关闭）
// 时连带杀掉作业里的所有进程。这比"退出时主动 stop"更可靠：强杀路径下根本
// 没有机会执行任何清理代码。
var (
	jobOnce sync.Once
	jobObj  windows.Handle
	jobErr  error
)

func ensureJob() (windows.Handle, error) {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			jobErr = fmt.Errorf("创建 Job Object 失败: %w", err)
			return
		}
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(
			h,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			_ = windows.CloseHandle(h)
			jobErr = fmt.Errorf("配置 Job Object 失败: %w", err)
			return
		}
		jobObj = h
	})
	return jobObj, jobErr
}

// adoptProcess 把子进程加进作业对象：agent 消失时它会一起被终止。
//
// 失败不是致命错误：那只是退回"没有这层保险"的老行为，
// 不能因为托管失败就让 agent 起不来。
func adoptProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	h, err := ensureJob()
	if err != nil {
		return err
	}
	const access = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE
	ph, err := windows.OpenProcess(access, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("打开子进程句柄失败: %w", err)
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(h, ph); err != nil {
		return fmt.Errorf("加入 Job Object 失败: %w", err)
	}
	return nil
}

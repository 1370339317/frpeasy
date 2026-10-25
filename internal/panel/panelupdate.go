package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"frpeasy/internal/shared"
)


// panelUpdateHandler POST /api/panel/update
//
// 立即返回 202，实际下载与替换放在后台：面板二进制约 11MB，慢网下要几十秒，
// 同步等待会让浏览器先超时，用户看到失败但后台其实成功了 —— 最糟的一种体验。
// 进展通过 <data>/update/state.json 暴露在 /api/version 的 last_update 里，
// UI 轮询它显示阶段；页面断开重连后仍然能看到结果（状态是落盘的）。
func (p *Panel) panelUpdateHandler(w http.ResponseWriter, r *http.Request) {
	p.shutdownMu.Lock()
	if p.shutdown {
		p.shutdownMu.Unlock()
		http.Error(w, "面板正在退出或更新中，请稍候", http.StatusConflict)
		return
	}
	if p.updating {
		p.shutdownMu.Unlock()
		http.Error(w, "已有一个更新在进行中", http.StatusConflict)
		return
	}
	p.updating = true
	p.shutdownMu.Unlock()

	fail := func(code int, msg string) {
		p.shutdownMu.Lock()
		p.updating = false
		p.shutdownMu.Unlock()
		http.Error(w, msg, code)
	}

	manifest, mErr := p.currentManifest(true)
	if mErr != "" {
		fail(http.StatusBadGateway, "检查更新失败: "+mErr)
		return
	}
	if manifest == nil {
		fail(http.StatusBadRequest, "尚未配置更新源。到「设置 → 版本与更新」填写更新源地址后再试")
		return
	}
	asset, err := manifest.SelectAsset(shared.AssetPanel, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if !shared.IsNewer(manifest.Version, p.version) {
		fail(http.StatusConflict, fmt.Sprintf("当前已是 %s，没有更新的版本（清单里是 %s）", p.version, manifest.Version))
		return
	}

	exe, err := os.Executable()
	if err != nil {
		fail(http.StatusInternalServerError, "无法定位面板二进制: "+err.Error())
		return
	}
	if !dirWritable(filepath.Dir(exe)) {
		fail(http.StatusForbidden, "面板所在目录不可写，无法替换二进制: "+filepath.Dir(exe))
		return
	}

	// 先把要做的决定告诉前端，然后后台干活
	json.NewEncoder(w).Encode(map[string]any{
		"started": true,
		"from":    p.version,
		"to":      manifest.Version,
		"message": "已开始下载新版本，完成后面板会自动重启，页面会自动重连",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go p.doPanelUpdate(manifest, asset, exe)
}


// doPanelUpdate 后台执行：下载 → 校验 → 预检 → 交接。
func (p *Panel) doPanelUpdate(manifest *shared.Manifest, asset *shared.Asset, exe string) {
	from := p.version
	to := manifest.Version
	dir := updateDir(p.dataDir)
	_ = os.MkdirAll(dir, 0o755)
	newPath := filepath.Join(dir, "panel.new")

	stage := func(s, detail string, errMsg string) {
		WriteUpdateState(p.dataDir, UpdateState{
			Stage: s, From: from, To: to, Detail: detail, Error: errMsg,
		})
		log.Printf("[update] %s: %s %s", s, detail, errMsg)
	}

	stage("downloading", "正在下载 "+to, "")
	url := manifest.URL(asset)
	if size, err := shared.DownloadFile(url, newPath); err != nil {
		stage("failed", "下载失败", err.Error())
		p.abortUpdate()
		return
	} else if size > maxPanelBinaryBytes {
		_ = os.Remove(newPath)
		stage("failed", "下载体积异常", fmt.Sprintf("下载了 %d 字节，超过上限 %d", size, maxPanelBinaryBytes))
		p.abortUpdate()
		return
	}

	// 校验：哈希是底线，长度是补充（清单没写 sha256 时至少能拦住截断）
	if asset.Size > 0 {
		if st, err := os.Stat(newPath); err != nil || st.Size() != asset.Size {
			_ = os.Remove(newPath)
			stage("failed", "文件大小不符", fmt.Sprintf("期望 %d 字节，实际 %v", asset.Size, err))
			p.abortUpdate()
			return
		}
	}
	if asset.SHA256 != "" {
		ok, err := shared.VerifyFileSHA256(newPath, asset.SHA256)
		if err != nil || !ok {
			_ = os.Remove(newPath)
			// 这一条必须硬失败：校验不过说明传输被截断或被篡改，
			// 装上去等于把面板换成一个不知道内容的程序。
			stage("failed", "sha256 校验未通过，已丢弃下载文件", fmt.Sprintf("期望 %s", asset.SHA256))
			p.abortUpdate()
			return
		}
	} else {
		// 没有哈希就装，等于放弃完整性校验。这里不拦（自建源可能真没算），
		// 但要在状态里写明白，让用户知道这次更新没有完整性保证。
		stage("downloading", "注意：清单未提供 sha256，本次更新无法校验完整性", "")
	}

	// 预检：能在本机跑起来、且报出的版本就是目标版本
	_ = os.Chmod(newPath, 0o755)
	if out, err := exec.Command(newPath, "-build").Output(); err != nil {
		_ = os.Remove(newPath)
		stage("failed", "新版本无法执行，已丢弃", err.Error())
		p.abortUpdate()
		return
	} else if !strings.Contains(string(out), to) {
		_ = os.Remove(newPath)
		stage("failed", "新版本自报的版本与清单不符，已丢弃",
			fmt.Sprintf("清单 %s，二进制自报 %s", to, strings.TrimSpace(string(out))))
		p.abortUpdate()
		return
	}

	// 交接
	stage("swapping", "准备交接给新版本", "")
	// 交接器用旧版副本，不是新下载的这份 —— 理由见 startHandoff 的注释：
	// 让未经验证的新代码去"拯救"旧进程，等于把最脆弱的一环放在最关键位置。
	handoffExe, err := prepareHandoffExe(exe, p.dataDir)
	if err != nil {
		_ = os.Remove(newPath)
		stage("failed", "准备交接器失败", err.Error())
		p.abortUpdate()
		return
	}
	if err := startHandoff(HandoffOptions{
		OldPID:      os.Getpid(),
		Listen:      p.listenAddr,
		DataDir:     p.dataDir,
		TargetExe:   exe,
		NewExe:      newPath,
		WantVersion: to,
		SelfExe:     handoffExe,
	}); err != nil {
		_ = os.Remove(newPath)
		stage("failed", "拉起交接进程失败", err.Error())
		p.abortUpdate()
		return
	}

	// 给 HTTP 响应和日志一点时间落盘，然后优雅退出。
	// 交接进程会等我们真的消失后才动手替换，所以这里不需要 wait 任何东西。
	go func() {
		time.Sleep(700 * time.Millisecond)
		p.Shutdown()
	}()
}


// abortUpdate 解除更新中的标记，允许用户重试。
func (p *Panel) abortUpdate() {
	p.shutdownMu.Lock()
	p.updating = false
	p.shutdownMu.Unlock()
}

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Goodbye 在卸载前向面板报备一次。
//
// 存在理由：无论走哪条卸载路径（本机 `install.sh --uninstall`、还是面板云端下发），
// agent 都是从面板的视野里直接消失。面板只能看到「心跳停了」——
// 而「它被卸载了」和「那台机器关机了」在数据上完全一样，于是记录永远挂着，
// 要人手动判断该不该清。
//
// 报备一次就能把这两件事分开：面板收到后立刻把该记录标成「已卸载（本机执行）」，
// 而不是等一次心跳超时再说「离线」。
//
// 失败不阻塞卸载：卸载的优先级高于报告自己成功。
func Goodbye(dataDir string) error {
	b, err := os.ReadFile(filepath.Join(dataDir, "agent.state"))
	if err != nil {
		return fmt.Errorf("读不到 agent.state（可能从未注册成功过）: %w", err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("agent.state 损坏: %w", err)
	}
	if s.Server == "" {
		// 老版本 agent 的 state 里没有 server 字段，无从报备。
		// 这不是错误，静默跳过即可 —— 面板那边会按普通离线处理。
		return fmt.Errorf("agent.state 里没有面板地址（老版本安装的），跳过报备")
	}
	return goodbyeTo(s.Server, s.AgentID, s.AgentTok)
}

// goodbye 用内存里的身份报备（云端下发卸载时走这条）。
func (c *Client) goodbye() error {
	return goodbyeTo(c.server, c.agentID, c.agentTok)
}

func goodbyeTo(server, id, tok string) error {
	if server == "" || id == "" || tok == "" {
		return fmt.Errorf("缺少面板地址或凭据")
	}
	body, _ := json.Marshal(map[string]string{"agent_id": id, "agent_token": tok})
	u := strings.TrimRight(server, "/") + "/api/agent/goodbye"
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// 超时要短：卸载流程在等这一下，不能因为面板网络慢把卸载拖住。
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("面板返回 %s", resp.Status)
	}
	return nil
}

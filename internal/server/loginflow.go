// loginflow.go 面板内 OAuth 登录（WorkBuddy CN state 流）。
// 把 cmd/login 的"发起授权 → 浏览器登录 → 轮询换 token → 拿账号信息"
// 搬进 HTTP 服务，凭证由 handler 层落盘 auths/ 并热加载进池。
// 多账号：每成功一次落盘 auths/workbuddy-<uid>.json 一个文件，
// 同 uid 重复登录则覆盖更新凭证（与 login.sh 行为一致）。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// 与 cmd/login/main.go 一致的常量（CN realm only）。
const (
	loginBase    = "https://copilot.tencent.com"
	loginUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
	loginOrigin  = "https://www.codebuddy.cn"
	loginStateEP = loginBase + "/v2/plugin/auth/state?platform=CLI"
	loginTokenEP = loginBase + "/v2/plugin/auth/token?state="
	loginAcctEP  = loginBase + "/v2/plugin/login/account?state="
	// loginSessionTTL 授权链接有效期；超时后 poll 返回 expired，需重新发起。
	loginSessionTTL = 10 * time.Minute
)

// loginFlow 管理当前进行中的登录会话。
// 面板单人场景：同一时刻只保留最新一个会话，重复 start 覆盖旧的。
type loginFlow struct {
	mu      sync.Mutex
	cur     *loginSession
	authDir string // 凭证落盘目录（由 config.auth_dir 注入）
}

// loginSession 一次进行中的登录；每会话独立 cookie jar（多账号互不串会话，同 cmd/login）。
type loginSession struct {
	mu        sync.Mutex
	state     string
	authURL   string
	client    *http.Client
	createdAt time.Time
}

// PollResult 是一次 poll 的结果。
type PollResult struct {
	Status  string // "pending" | "done" | "expired" | "error"
	Message string
	Auth    *auth.Auth // status==done 时非空，由调用方落盘 + 入池
}

func newLoginFlow(authDir string) *loginFlow {
	return &loginFlow{authDir: authDir}
}

// loginHeaders 与 cmd/login.commonHeaders 一致。
func loginHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", loginOrigin)
	req.Header.Set("Referer", loginOrigin+"/")
	req.Header.Set("User-Agent", loginUA)
}

// doEnvelope 与 cmd/login.doJSON 一致：{code,msg,data} 信封，code!=0 → error。
func doEnvelope(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		loginHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// Start 发起一次登录：POST auth/state 拿 state + 授权 URL。
func (f *loginFlow) Start() (authURL, state string, err error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	data, _, err := doEnvelope(client, http.MethodPost, loginStateEP, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", fmt.Errorf("auth state: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: missing state or authUrl")
	}
	f.mu.Lock()
	f.cur = &loginSession{state: st.State, authURL: st.AuthURL, client: client, createdAt: time.Now()}
	f.mu.Unlock()
	return st.AuthURL, st.State, nil
}

// Poll 对当前会话做一次 token 轮询。
//   - 上游业务 code!=0（"login ing"）→ pending（用户还没在浏览器完成登录）
//   - code=0 + token bundle → 拿 account（uid/nickname）构造 Auth 返回 done
//   - 会话超时 → expired
func (f *loginFlow) Poll(state string) PollResult {
	f.mu.Lock()
	s := f.cur
	f.mu.Unlock()
	if s == nil {
		return PollResult{Status: "error", Message: "没有进行中的登录，请先点「开始登录」"}
	}
	if state != "" && s.state != state {
		return PollResult{Status: "error", Message: "登录会话已更新（可能重新发起过），请刷新后用最新会话"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.createdAt) > loginSessionTTL {
		f.mu.Lock()
		f.cur = nil
		f.mu.Unlock()
		return PollResult{Status: "expired", Message: "授权已超时（10 分钟），请重新点「开始登录」"}
	}

	// auth/token 是权威登录状态端点：pending 时业务 code 非 0，完成时 code=0 + token bundle
	tokRaw, status, errTok := doEnvelope(s.client, http.MethodGet, loginTokenEP+s.state, nil, nil)
	if errTok != nil {
		if status == 0 || status >= 500 {
			return PollResult{Status: "error", Message: "token 端点错误: " + errTok.Error()}
		}
		return PollResult{Status: "pending"}
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return PollResult{Status: "pending"}
	}

	// login/account 拿 uid/nickname（带 Bearer；失败不阻塞，仅无法落盘）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		loginHeaders(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doEnvelope(s.client, http.MethodGet, loginAcctEP+s.state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	// 会话已消费，清掉
	f.mu.Lock()
	f.cur = nil
	f.mu.Unlock()

	if acct.UID == "" {
		return PollResult{Status: "error", Message: "登录成功但未能获取 uid（account 接口失败），请重新登录"}
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Unix() + tok.ExpiresIn,
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     f.authDir + "/workbuddy-" + acct.UID + ".json",
	}
	return PollResult{Status: "done", Auth: a}
}

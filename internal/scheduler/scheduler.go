// Package scheduler 定时任务：每日签到（默认凌晨 01 点）+ token keepalive（22点）。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [1]（凌晨 1 点全量签到）
	KeepaliveHours []int // 默认 [22]
}

// CheckinResult 单账号签到结果（供面板 /api/checkin 逐行展示）。
type CheckinResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Checkin  string `json:"checkin"` // OK | ALREADY | FAIL | SKIP
	Remain   int64  `json:"remain"`  // -1 = 未获取到余额
	Detail   string `json:"detail,omitempty"`
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{1}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	return &Scheduler{cfg: cfg}
}

// CheckinHoursSnapshot 返回签到小时配置副本（供面板展示）。
func (s *Scheduler) CheckinHoursSnapshot() []int {
	return append([]int{}, s.cfg.CheckinHours...)
}

// KeepaliveHoursSnapshot 返回 keepalive 小时配置副本。
func (s *Scheduler) KeepaliveHoursSnapshot() []int {
	return append([]int{}, s.cfg.KeepaliveHours...)
}

// NextCheckinAt 返回下一次自动签到时间（本地时区）。
func (s *Scheduler) NextCheckinAt() time.Time {
	return NextFire(time.Now(), s.cfg.CheckinHours)
}

// NextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
// 导出供面板计算"下次自动签到"倒计时。
func NextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	all := append(append([]int{}, s.cfg.CheckinHours...), s.cfg.KeepaliveHours...)
	for {
		next := NextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.CheckinHours, h) {
				s.RunCheckinNow()
			}
			if contains(s.cfg.KeepaliveHours, h) {
				s.RunKeepaliveNow()
			}
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻（打日志版，定时触发走这里）。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	for _, r := range s.RunCheckinReported() {
		switch r.Checkin {
		case "FAIL", "SKIP":
			log.Printf("checkin %s: %s %s", r.UID, r.Checkin, r.Detail)
		case "OK":
			log.Printf("checkin %s: OK remain=%d", r.UID, r.Remain)
		}
	}
}

// RunCheckinReported 立即全量签到并返回逐账号结果（供面板手动触发展示）。
// 语义与 RunCheckinNow 一致：签到 → 查余额 → ReenableIfCredits 解冻。
func (s *Scheduler) RunCheckinReported() []CheckinResult {
	out := make([]CheckinResult, 0)
	for _, st := range s.cfg.Pool.List() {
		r := CheckinResult{UID: st.UID, Remain: -1}
		if st.Disabled {
			r.Checkin, r.Detail = "SKIP", "账号已禁用"
			out = append(out, r)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			r.Checkin, r.Detail = "SKIP", "无 refresh token"
			out = append(out, r)
			continue
		}
		r.Nickname = a.Nickname
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			// 已签到是幂等业务错误，不算失败
			if isAlreadyCheckin(err.Error()) {
				r.Checkin, r.Detail = "ALREADY", err.Error()
			} else {
				r.Checkin, r.Detail = "FAIL", err.Error()
			}
		} else {
			r.Checkin = "OK"
		}
		if remain, err := s.cfg.Upstream.UserResource(a); err != nil {
			r.Detail = joinDetail(r.Detail, "user-resource: "+err.Error())
			log.Printf("user-resource %s: %v", st.UID, err)
		} else {
			r.Remain = remain
			s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		}
		out = append(out, r)
	}
	return out
}

// isAlreadyCheckin 与 cmd/signin 的判定一致：已签到等幂等业务错误。
func isAlreadyCheckin(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "12153 session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
	}
}

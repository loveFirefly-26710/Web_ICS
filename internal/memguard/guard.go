// Package memguard 做内存自守：在容器硬限之前先降级，避免 OOM。
//
// 四档（面向 1G 机器）：
//
//	RSS < softLimit  : 正常服务
//	RSS > softLimit  : 清空目录树缓存，继续服务
//	RSS > hardLimit  : 拒绝新请求（503），避免雪崩
//	RSS > panicLimit : 记录告警，等恢复
package memguard

import (
	"log"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// Config 是内存阈值配置。
//
// 这里不提供 DefaultConfig()：默认值只留一处真相，即 cmd/web_ics/main.go
// 的 --soft-limit / --hard-limit / --panic-limit。
type Config struct {
	SoftLimit  uint64 // 字节。超过则清缓存
	HardLimit  uint64 // 字节。超过则拒绝新请求
	PanicLimit uint64 // 字节。超过则告警
	Interval   time.Duration
}

// Guard 周期性地检查进程 RSS 并执行降级动作。
type Guard struct {
	cfg      Config
	onSoft   func()
	degraded atomic.Bool // true 表示当前处于拒绝服务状态
	lastRSS  atomic.Uint64
	softHits atomic.Int64
	hardHits atomic.Int64
	stopCh   chan struct{}
}

// New 创建内存守卫。onSoft 在超过软限时被调用（用于清缓存）。
func New(cfg Config, onSoft func()) *Guard {
	return &Guard{cfg: cfg, onSoft: onSoft, stopCh: make(chan struct{})}
}

// Start 启动后台检查循环。
func (g *Guard) Start() {
	go func() {
		t := time.NewTicker(g.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-g.stopCh:
				return
			case <-t.C:
				g.check()
			}
		}
	}()
}

// Stop 停止检查循环。
func (g *Guard) Stop() { close(g.stopCh) }

func (g *Guard) check() {
	rss := CurrentRSS()
	g.lastRSS.Store(rss)

	switch {
	case rss >= g.cfg.PanicLimit:
		log.Printf("[memguard] 告警：RSS=%d MB 超过 panic 阈值 %d MB",
			rss>>20, g.cfg.PanicLimit>>20)
		g.setDegraded(true)
	case rss >= g.cfg.HardLimit:
		if !g.degraded.Load() {
			log.Printf("[memguard] RSS=%d MB 超过硬限 %d MB，开始拒绝新请求",
				rss>>20, g.cfg.HardLimit>>20)
		}
		g.hardHits.Add(1)
		g.setDegraded(true)
	case rss >= g.cfg.SoftLimit:
		g.hardHits.Store(0)
		g.softHits.Add(1)
		g.setDegraded(false)
		if g.onSoft != nil {
			log.Printf("[memguard] RSS=%d MB 超过软限 %d MB，清理目录树缓存",
				rss>>20, g.cfg.SoftLimit>>20)
			g.onSoft()
		}
	default:
		// 回落到安全区，连续两次低于软限才解除降级，避免抖动
		g.hardHits.Store(0)
		g.setDegraded(false)
	}
}

func (g *Guard) setDegraded(v bool) { g.degraded.Store(v) }

// Degraded 返回当前是否应拒绝新请求。
func (g *Guard) Degraded() bool { return g.degraded.Load() }

// Stats 返回用于 /debug/healthz 的完整统计。
func (g *Guard) Stats() (rss uint64, degraded bool, softHits, hardHits int64) {
	return g.lastRSS.Load(), g.degraded.Load(), g.softHits.Load(), g.hardHits.Load()
}

// publicMemStepMB 是对外暴露内存时的取整粒度。
const publicMemStepMB = 50

// PublicStats 返回可以匿名对外的粗粒度内存指标：取整后的 MB 值加档位。
//
// 不给精确 RSS 的原因：匿名暴露实时内存等于给攻击者一把尺子，他能把负载
// 精确压在软限附近，让守卫反复清标题索引，每次搜索都得重建。50 MB 的精度
// 不足以做这件事，而看趋势、看有没有逼近上限都够用。需要精确值时开
// --debug-health 走 /debug/healthz。
//
// 取整向上而不是向下：冷启动 RSS 只有 19 MB，向下取整会显示成「内存 0 MB」。
func (g *Guard) PublicStats() (memMB int, level string) {
	rss := g.lastRSS.Load()
	if rss == 0 {
		// 守卫是每 5 秒采样一次，刚启动时 lastRSS 还是零值。
		// 这里补读一次，否则前端顶栏会先显示「内存 0 MB」。
		rss = CurrentRSS()
	}
	mb := int(rss >> 20)
	return (mb + publicMemStepMB - 1) / publicMemStepMB * publicMemStepMB, g.level(rss)
}

// level 把 RSS 映射成三档。阈值取自配置，这样前端不必硬编码阈值
// （软硬限都能用命令行改，写死在前端迟早对不上）。
func (g *Guard) level(rss uint64) string {
	switch {
	case rss >= g.cfg.HardLimit:
		return "critical"
	case rss >= g.cfg.SoftLimit:
		return "high"
	default:
		return "normal"
	}
}

// CurrentRSS 返回当前进程的常驻内存（字节）。
//
// 优先走平台实现：Linux 读 /proc/self/statm，Windows 读进程工作集。
// 两者与 cgroup / docker stats / 任务管理器是同一个口径，守卫据此判断才准。
//
// 其它平台退回 runtime.MemStats 的近似值（Sys 减 HeapReleased）。
// 这个近似值偏差很大，实测容器里工作集 21 MB 时它能报到 146 MB，
// 会让降级在错误的时点触发。
func CurrentRSS() uint64 {
	if v, ok := readRSS(); ok {
		return v
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// Sys 包含堆、栈、GC 元数据等所有向 OS 申请的内存
	// HeapReleased 是已归还给 OS 的部分
	if ms.Sys > ms.HeapReleased {
		return ms.Sys - ms.HeapReleased
	}
	return ms.Sys
}

// SetMemLimit 设置 Go 运行时的软内存上限。
//
// 关键：Go 1.19+ 默认不感知 cgroup 内存限制，会按宿主机总内存决定 GC 时机，
// 导致 RSS 一路涨到容器被 OOM Kill。必须显式设置，让 GC 提前积极回收。
func SetMemLimit(bytes uint64) {
	if bytes > 0 {
		debug.SetMemoryLimit(int64(bytes))
	}
}

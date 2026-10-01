// Command web_ics 是一个轻量级的产品文档阅读器，读取华为 HDX/HWICS 文档包。
// 目标是在 2 核 1G 内存的服务器上稳定运行。
//
// 用法：
//
//	web_ics --doc-root /docs --port 8080
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"web_ics/internal/corpus"
	"web_ics/internal/doclib"
	"web_ics/internal/memguard"
	"web_ics/internal/nav"
	"web_ics/internal/server"
)

// version 是版本号。默认值供本地开发使用，发布时由 build.sh 与 CI 通过
// -ldflags "-X main.version=<tag>" 覆盖，不要在这里手工改。
var version = "0.1.0"

func main() {
	// 所有 flag 的默认值都填内置默认，不带环境变量也不带配置文件。
	// 环境变量与配置文件在 flag.Parse 之后由 applySettings 统一按优先级补：
	// 命令行 > 环境变量 > 配置文件 > 内置默认。
	var (
		configPath = flag.String("config", "",
			"配置文件路径。不指定就依次找可执行文件旁边、用户配置目录")
		docRoot   = flag.String("doc-root", "./docs", "文档库目录（含 .hdx/.hwics）")
		addr      = flag.String("addr", "", "监听地址，如 :8080")
		port      = flag.Int("port", 8080, "监听端口（addr 未指定时生效）")
		maxConc   = flag.Int("max-concurrency", 10, "最大在途请求数")
		memLimitM = flag.Int("mem-limit", 300, "Go 运行时软内存上限（MB），0 表示不限制")
		// 阈值必须明显高于真实稳态，否则守卫会在稳态上反复触发：
		// 释放标题索引，下次搜索重建（数秒），又超阈值，再释放。
		// 冷启动 19 MB，建完标题索引 310 MB。
		softM     = flag.Int("soft-limit", 350, "清理缓存的 RSS 阈值（MB）")
		hardM     = flag.Int("hard-limit", 500, "拒绝新请求的 RSS 阈值（MB）")
		panicM    = flag.Int("panic-limit", 450, "告警的 RSS 阈值（MB）")
		showVer   = flag.Bool("version", false, "打印版本后退出")
		selfTest  = flag.Bool("selftest", false, "运行内建自检（不起常驻服务）后退出")
		corpusDir = flag.String("corpus-dir", "",
			"正文语料目录（默认 <doc-root>/.web_ics-corpus）")
		buildCorpus = flag.Bool("build-corpus", false, "只建正文语料后退出")
		ipRate      = flag.Float64("ip-rate", 5,
			"每 IP 每秒允许的请求数，0 表示不限流")
		ipBurst = flag.Int("ip-burst", 20,
			"每 IP 的突发容量")
		trustProxy = flag.Bool("trust-proxy", false,
			"信任 X-Forwarded-For 作为客户端 IP（只有前面挂了反代才该开）")
		debugHealth = flag.Bool("debug-health", false,
			"暴露 /debug/healthz 详细指标（默认关闭）")
		// ---- 客户端证书认证（ADR-17）----
		// 四个键里 client-ca 是唯一的开关：不配它就是原来的裸 HTTP。
		tlsCert = flag.String("tls-cert", "",
			"服务器证书 PEM。配了 client-ca 时必填")
		tlsKey = flag.String("tls-key", "",
			"服务器私钥 PEM。配了 client-ca 时必填")
		clientCA = flag.String("client-ca", "",
			"信任的客户端 CA 证书 PEM（可含多张）。配了就要求客户端证书，服务改用 HTTPS")
		clientCertDeny = flag.String("client-cert-deny", "",
			"客户端证书吊销名单，每行一个 sha256 指纹")
		// Windows 上默认开：这个平台上它是双击即用的桌面工具，不开浏览器
		// 用户还得自己找地址。Linux 上默认关，那边是服务器，多半没有桌面环境。
		// 环境变量 WEB_ICS_OPEN_BROWSER 与配置文件里的 open 都能覆盖它。
		openUI = flag.Bool("open", runtime.GOOS == "windows",
			"启动后用系统默认浏览器打开界面（Windows 上默认开）")
	)
	flag.Parse()

	// --version 不读配置也不生成模板：它应当无副作用，随时可跑。
	if *showVer {
		// runtime.Version() 本身带 go 前缀，格式串里不再重复写。
		fmt.Printf("web_ics %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[web_ics] ")

	// 环境变量与配置文件在这里补进 flag。必须赶在读任何 flag 值之前。
	cf := loadConfig(*configPath)
	applySettings(cf)
	cf.logSummary()

	// ---- 客户端证书认证 ----
	//
	// client-ca 是唯一的开关。配了它却没配证书文件就直接退出，不退化成
	// 「以为开了其实没开」。这段刻意放在扫描文档库之前：证书配错了要立刻
	// 知道，而不是等语料建到一半才失败。
	var clientAuth *server.ClientAuth
	var tlsCfg *tls.Config
	if *clientCA != "" {
		if *tlsCert == "" || *tlsKey == "" {
			log.Fatalf("配了 client-ca 就必须同时配 tls-cert 与 tls-key")
		}
		ca, err := server.LoadClientAuth(*clientCA, *clientCertDeny)
		if err != nil {
			log.Fatalf("加载客户端证书配置失败: %v", err)
		}
		clientAuth = ca
		tc, err := ca.TLSConfig(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("加载服务器证书失败: %v", err)
		}
		tlsCfg = tc
		log.Printf("客户端证书认证已启用（CA: %s），服务走 HTTPS", *clientCA)
		if *clientCertDeny != "" {
			log.Printf("客户端证书吊销名单: %s", *clientCertDeny)
		}
	}

	if *selfTest {
		printEnv(*docRoot, cf)
		os.Exit(runSelfTest(*docRoot))
	}

	// ---- 限制 Go 运行时资源，避免按宿主机内存大手大脚 ----
	debug.SetGCPercent(50) // 更积极的 GC，代价是少量 CPU
	if *memLimitM > 0 {
		memguard.SetMemLimit(uint64(*memLimitM) << 20)
		log.Printf("Go 软内存上限设为 %d MB", *memLimitM)
	}
	// 核数少的机器上不要用满所有核去跑 GC 与调度
	if os.Getenv("GOMAXPROCS") == "" {
		n := runtime.NumCPU()
		if n > 2 {
			n = 2
		}
		runtime.GOMAXPROCS(n)
	}
	log.Printf("GOMAXPROCS=%d, CPU=%d", runtime.GOMAXPROCS(0), runtime.NumCPU())

	// ---- 扫描文档库（只读元数据，不解析目录树）----
	t0 := time.Now()
	index, err := doclib.Scan(*docRoot)
	if err != nil {
		// 不退出。文档库目录不存在或读不了时照样起服务，界面会显示 0 个包，
		// 原因写在这条日志里。退出的话双击启动的控制台一闪而过，
		// 用户什么也看不到。
		log.Printf("扫描文档库失败: %v", err)
		log.Printf("将以空文档库启动。改 --doc-root，或配置文件里的 doc-root")
		index = doclib.Empty(*docRoot)
	}
	libs := index.List()
	var totalBytes int64
	for _, l := range libs {
		totalBytes += l.SizeBytes
	}
	log.Printf("文档库: %s", *docRoot)
	log.Printf("发现 %d 个文档包，共 %.1f GB，索引耗时 %v",
		len(libs), float64(totalBytes)/(1<<30), time.Since(t0).Round(time.Millisecond))
	if len(libs) == 0 {
		log.Printf("警告：未找到任何 .hdx/.hwics 文件，请检查 --doc-root 是否正确")
	}

	// ---- 正文语料库（正文全文检索的地基）----
	//
	// 建语料要解压全部 82.8 万篇 HTML、约 4 分钟，所以不能放在启动路径上：
	// 启动时只做「缺失或过期」检查（几十次 stat，毫秒级），把重建丢到后台
	// 逐个做。建好之前该包的正文不参与检索，搜索响应里如实报告「语料 X/N」，
	// 而不是退回「只扫一个包」那种会让用户误判的半成品。
	var corpusStore *corpus.Store
	if len(libs) > 0 {
		corpusStore = corpus.NewStore(*corpusDir, libs)
		missing := corpusStore.Refresh()
		log.Printf("正文语料: %s（%d/%d 已就绪）", corpusStore.Dir(),
			corpusStore.ReadyCount(), corpusStore.Total())
		if *buildCorpus {
			corpusStore.BuildAll(missing, log.Printf)
			return
		}
		if len(missing) > 0 {
			log.Printf("后台建立正文语料：%d 个包待建（约 4 分钟；期间正文检索只覆盖已建好的包）",
				len(missing))
			go corpusStore.BuildAll(missing, log.Printf)
		}
	}

	// ---- 目录树缓存 + 内存守卫 ----
	trees := nav.NewCache()
	guardCfg := memguard.Config{
		SoftLimit:  uint64(*softM) << 20,
		HardLimit:  uint64(*hardM) << 20,
		PanicLimit: uint64(*panicM) << 20,
		Interval:   5 * time.Second,
	}

	// 服务器先建，以便内存守卫在软限时也能释放搜索索引。
	cfg := server.DefaultConfig()
	cfg.MaxConcurrency = *maxConc
	cfg.IPRate = *ipRate
	cfg.IPBurst = *ipBurst
	cfg.TrustProxy = *trustProxy
	cfg.DebugHealth = *debugHealth
	cfg.ClientAuth = clientAuth
	if *addr == "" {
		*addr = fmt.Sprintf(":%d", *port)
	}
	cfg.Addr = *addr
	srv := server.New(cfg, index, trees, nil)
	srv.SetCorpus(corpusStore)

	guard := memguard.New(guardCfg, func() {
		trees.Drop() // 超过软限时清空目录树缓存
		// 标题索引是最大的一块内存（约 290 MB），压力下优先释放它：
		// 代价是下次搜索要重新扫 navi.xml（数秒），但比 OOM 好。
		if si := srv.SearchIndex(); si != nil {
			si.Release()
		}
	})
	srv.SetGuard(guard)
	guard.Start()
	defer guard.Stop()
	log.Printf("内存守卫: 软限 %d MB / 硬限 %d MB / 告警 %d MB",
		*softM, *hardM, *panicM)

	// ---- 启动 HTTP 服务 ----
	httpSrv := server.NewHTTPServer(cfg.Addr, srv.Handler(), tlsCfg)

	go func() {
		log.Printf("监听 %s", cfg.Addr)
		// 开了客户端证书认证就走 https，否则浏览器会被开到连不上的地址上。
		scheme := "http"
		if tlsCfg != nil {
			scheme = "https"
		}
		uiURL := scheme + "://127.0.0.1" + portSuffix(cfg.Addr)
		log.Printf("打开浏览器访问 %s", uiURL)
		if *openUI {
			// 稍等一下再开，免得浏览器抢在 ListenAndServe 之前发请求。
			// 失败只记日志：没有桌面环境的机器上本来就打不开。
			go func() {
				time.Sleep(400 * time.Millisecond)
				if err := openURL(uiURL); err != nil {
					log.Printf("自动打开浏览器失败（不影响服务）: %v", err)
				}
			}()
		}
		var err error
		if tlsCfg != nil {
			// 证书已经在 tlsCfg.Certificates 里，不用再传文件名。
			err = httpSrv.ListenAndServeTLS("", "")
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && err.Error() != "http: Server closed" {
			log.Fatalf("HTTP 服务异常: %v", err)
		}
	}()

	// ---- 等待退出信号 ----
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("收到退出信号，正在关闭...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx, httpSrv); err != nil {
		log.Printf("关闭超时: %v", err)
	}
	rss := memguard.CurrentRSS()
	log.Printf("已退出。最终 RSS=%d MB", rss>>20)
	debug.FreeOSMemory()
}

// portSuffix 从监听地址里取出「:端口」那一段，用来拼浏览器访问地址。
//
// 地址有三种写法：":8080"、"127.0.0.1:18098"、"[::1]:8080"。只判断
// addr[0] == ':' 会把后两种漏掉，退回硬编码的 8080，于是 --addr 换了端口
// 之后 --open 打开的还是 8080，页面上不来说明不清。
func portSuffix(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return ":" + port
	}
	return ":8080"
}

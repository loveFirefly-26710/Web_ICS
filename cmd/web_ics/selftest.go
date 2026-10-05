package main

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"web_ics/internal/corpus"
	"web_ics/internal/doclib"
	"web_ics/internal/nav"
	"web_ics/internal/search"
	"web_ics/internal/server"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// runSelfTest 在进程内起一个测试服务器，逐个验证关键路径。
//
// 用途：在没有浏览器、甚至没有网络的环境里确认二进制可用。
// 交付产物发布前应跑一遍：web_ics --selftest --doc-root <dir>
func runSelfTest(docRoot string) int {
	fmt.Println("=== 自检开始 ===")
	fail := 0
	check := func(name string, ok bool, detail string) {
		status := "PASS"
		if !ok {
			status = "FAIL"
			fail++
		}
		if detail != "" {
			fmt.Printf("[%s] %-42s %s\n", status, name, detail)
		} else {
			fmt.Printf("[%s] %-42s\n", status, name)
		}
	}

	// 1. 扫描
	t0 := time.Now()
	index, err := doclib.Scan(docRoot)
	if err != nil {
		fmt.Printf("[FAIL] 扫描文档库失败: %v\n", err)
		return 1
	}
	libs := index.List()
	check("扫描文档库", len(libs) > 0, fmt.Sprintf("%d 个包, 耗时 %v", len(libs), time.Since(t0).Round(time.Millisecond)))
	if len(libs) == 0 {
		fmt.Println("=== 自检失败 ===")
		return 1
	}

	// 2. 构造测试服务器
	cache := nav.NewCache()
	cfg := server.DefaultConfig()
	cfg.DebugHealth = true // 自检要读详细指标
	cfg.IPRate = 0         // 自检会连打几十个请求，限流会误伤
	srv := server.New(cfg, index, cache, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	get := func(path string) (int, []byte, http.Header) {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			return 0, nil, nil
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return resp.StatusCode, b, resp.Header
	}

	// 3. 库列表
	code, body, _ := get("/api/libs")
	check("GET /api/libs", code == 200 && strings.Contains(string(body), libs[0].LibID),
		fmt.Sprintf("status=%d bytes=%d", code, len(body)))

	// 4. 首页与静态资源
	code, body, _ = get("/")
	check("GET / (首页)", code == 200 && strings.Contains(string(body), "Web_ICS"), fmt.Sprintf("status=%d bytes=%d", code, len(body)))

	// 5. 对若干库验证：阅读页 + 顶层目录 + 首篇正文
	tested := 0
	for _, l := range libs {
		if tested >= 5 {
			break
		}
		// 阅读页。顺手断言品牌字：首页与阅读页两个外壳各写一遍，
		// 改名时最容易只改一个，光看 HTTP 200 是发现不了的。
		code, body, _ = get("/doc/" + l.LibID)
		if code != 200 {
			check("GET /doc/"+l.LibID, false, fmt.Sprintf("status=%d", code))
			continue
		}
		if tested == 0 {
			check("阅读页带品牌字", strings.Contains(string(body), "Web_ICS"),
				fmt.Sprintf("bytes=%d", len(body)))
		}

		// 顶层目录
		code, body, _ = get("/api/nav?lib=" + l.LibID + "&parent=")
		if code != 200 {
			check("nav 顶层 "+l.LibID, false, fmt.Sprintf("status=%d", code))
			continue
		}
		var navResp struct {
			Items []nav.Node `json:"items"`
		}
		if err := jsonUnmarshal(body, &navResp); err != nil {
			check("nav 解析 "+l.LibID, false, err.Error())
			continue
		}
		if len(navResp.Items) == 0 {
			check("nav 顶层非空 "+l.LibID, false, "顶层节点为 0")
			continue
		}

		// 5b. 目录深链：从搜索结果直接进入某篇文档时，阅读页需要
		//     「根 -> 目标」的节点链才能把目录树逐层展开并选中。
		//     标题命中带 nodeId（按 node 解析），正文命中只有 URL（按 url 反查）。
		if tested == 0 && navResp.Items[0].URL != "" {
			top0 := navResp.Items[0]
			var byNode, byURL struct {
				Path []nav.Node `json:"path"`
			}
			code, body, _ = get("/api/nav?lib=" + l.LibID + "&node=" + urlEscape(top0.ID))
			okNode := code == 200 && jsonUnmarshal(body, &byNode) == nil &&
				len(byNode.Path) >= 1 && byNode.Path[len(byNode.Path)-1].ID == top0.ID
			check("目录深链按 node 解析", okNode,
				fmt.Sprintf("status=%d path=%d", code, len(byNode.Path)))

			code, body, _ = get("/api/nav?lib=" + l.LibID + "&url=" + urlEscape(top0.URL))
			okURL := code == 200 && jsonUnmarshal(body, &byURL) == nil &&
				len(byURL.Path) >= 1 && byURL.Path[len(byURL.Path)-1].URL == top0.URL
			check("目录深链按 url 反查", okURL,
				fmt.Sprintf("status=%d path=%d", code, len(byURL.Path)))

			// 不存在的节点必须返回空链而不是报错，前端据此退化成打开第一篇
			code, body, _ = get("/api/nav?lib=" + l.LibID + "&node=" + urlEscape("不存在的节点"))
			var none struct {
				Path []nav.Node `json:"path"`
			}
			check("目录深链对未知节点返回空链",
				code == 200 && jsonUnmarshal(body, &none) == nil && len(none.Path) == 0,
				fmt.Sprintf("status=%d path=%d", code, len(none.Path)))
		}

		// 首篇正文直出
		first := navResp.Items[0]
		if first.URL != "" && isDocHTML(first.URL) {
			code, body, hdr := get("/doc/" + l.LibID + "/res/" + first.URL)
			ct := hdr.Get("Content-Type")
			ok := code == 200 && len(body) > 0 && !strings.Contains(ct, "charset=utf-8")
			check("正文直出 "+truncStr(l.LibName, 20), ok,
				fmt.Sprintf("status=%d bytes=%d ct=%q", code, len(body), ct))
		}
		tested++
	}
	check("抽样库端到端", tested > 0, fmt.Sprintf("验证了 %d 个库", tested))

	// 6. 静态资源路由
	code, _, _ = get("/static/viewer.js")
	check("GET /static/viewer.js", code == 200, fmt.Sprintf("status=%d", code))
	code, _, hdr2 := get("/static/style.css")
	check("GET /static/style.css", code == 200, fmt.Sprintf("status=%d", code))
	// 缓存策略回归：静态资源必须 revalidate（no-cache），不能是 max-age 盲缓存，
	// 否则改完 CSS 刷新拿不到新版
	check("静态资源可协商缓存", headerHas(hdr2, "Cache-Control", "no-cache"),
		"style.css Cache-Control="+hdr2.Get("Cache-Control"))
	// HTML 外壳必须完全不缓存
	code, _, hdrHome := get("/")
	check("首页外壳不缓存",
		code == 200 && headerHas(hdrHome, "Cache-Control", "no-store"),
		"Cache-Control="+hdrHome.Get("Cache-Control"))

	// 7. 路径穿越防护
	code, _, _ = get("/doc/" + libs[0].LibID + "/res/../../../../windows/win.ini")
	check("路径穿越被拒", code == 404, fmt.Sprintf("status=%d (期望 404)", code))

	// 7b. 只允许读请求
	code, _, _ = post(ts.URL, "/api/search?q=abc")
	check("非 GET/HEAD 被拒", code == 405, fmt.Sprintf("status=%d (期望 405)", code))

	// 8. 健康检查：对外只回答 ok/degraded 加粗粒度内存，详细指标在 /debug/healthz
	code, body, hdrHealth := get("/healthz")
	check("GET /healthz 只暴露粗粒度指标",
		code == 200 && strings.Contains(string(body), "\"ok\"") &&
			strings.Contains(string(body), "\"memMB\"") &&
			!strings.Contains(string(body), "rssMB") &&
			!strings.Contains(string(body), "corpusErr"),
		fmt.Sprintf("status=%d body=%s", code, truncate(string(body), 90)))
	code, body, _ = get("/debug/healthz")
	check("GET /debug/healthz 有详细指标",
		code == 200 && strings.Contains(string(body), "rssMB"),
		fmt.Sprintf("status=%d", code))

	// 8b. 安全响应头
	check("安全头 nosniff", hdrHealth.Get("X-Content-Type-Options") == "nosniff",
		"X-Content-Type-Options="+hdrHealth.Get("X-Content-Type-Options"))
	check("安全头 X-Frame-Options", hdrHealth.Get("X-Frame-Options") == "SAMEORIGIN",
		"X-Frame-Options="+hdrHealth.Get("X-Frame-Options"))
	check("安全头 Referrer-Policy", hdrHealth.Get("Referrer-Policy") == "no-referrer",
		"Referrer-Policy="+hdrHealth.Get("Referrer-Policy"))
	check("安全头 CSP（主站严格）",
		strings.Contains(hdrHealth.Get("Content-Security-Policy"), "frame-ancestors 'self'") &&
			strings.Contains(hdrHealth.Get("Content-Security-Policy"), "object-src 'none'"),
		"CSP="+truncate(hdrHealth.Get("Content-Security-Policy"), 70))

	// 8c. 正文路由的 CSP 必须放宽（大量正文自带内联 script）。
	//     这里不挑具体资源：头是中间件按路由设的，响应码无关紧要。
	//     正文实际能不能渲染由浏览器端验证。
	_, _, hdrDoc := get("/doc/" + libs[0].LibID + "/res/任意资源.html")
	docCSP := hdrDoc.Get("Content-Security-Policy")
	check("正文路由 CSP 放宽且仍禁嵌套",
		strings.Contains(docCSP, "unsafe-inline") &&
			strings.Contains(docCSP, "frame-ancestors 'self'"),
		"CSP="+truncate(docCSP, 70))

	// 8d. 参数边界：超长检索词应被拒
	longQ := strings.Repeat("a", 300)
	code, _, _ = get("/api/search?q=" + longQ)
	check("超长检索词被拒（search）", code == 400, fmt.Sprintf("status=%d (期望 400)", code))
	code, _, _ = get("/api/grep?q=" + longQ)
	check("超长检索词被拒（grep）", code == 400, fmt.Sprintf("status=%d (期望 400)", code))

	// 8e. 错误响应不回显内部路径
	code, body, _ = get("/doc/" + libs[0].LibID + "/res/nope/nope.html")
	check("错误响应不回显内部路径",
		code == 404 && !strings.Contains(string(body), "resources/"),
		fmt.Sprintf("status=%d body=%s", code, truncate(string(body), 70)))

	// 8f. 目录解析失败时不回显服务端路径
	code, body, _ = get("/api/nav?lib=" + urlEscape("不存在的库"))
	check("未知库不泄露内部信息",
		code == 404 && !strings.Contains(string(body), "/"),
		fmt.Sprintf("status=%d body=%s", code, truncate(string(body), 60)))

	// 9. 不存在的资源应 404
	code, _, _ = get("/doc/" + libs[0].LibID + "/res/不存在的文件.html")
	check("不存在的资源返回 404", code == 404, fmt.Sprintf("status=%d", code))

	// 10. 大小写不敏感回退
	// 文档包里的 URL 大小写与包内文件名经常不一致（全库 26.4% 的目录 URL
	// 只能靠忽略大小写命中）。这里挑一个已知案例验证回退链路真的通。
	caseChecked := 0
	for _, l := range libs {
		if caseChecked >= 2 {
			break
		}
		cases := []string{
			"/doc/" + l.LibID + "/res/public_sys-resources/tabSection.js",
			"/doc/" + l.LibID + "/res/public_sys-resources/customQuery.js",
		}
		for _, c := range cases {
			code, body, _ := get(c)
			if code == 200 && len(body) > 0 {
				caseChecked++
				break
			}
		}
	}
	check("大小写不敏感回退", caseChecked > 0,
		fmt.Sprintf("验证了 %d 个库的 tabSection.js/customQuery.js", caseChecked))

	// 11. 分类接口
	code, body, _ = get("/api/categories")
	var catResp struct {
		Total int               `json:"total"`
		Items []doclib.Category `json:"items"`
	}
	catOK := code == 200 && jsonUnmarshal(body, &catResp) == nil && catResp.Total > 0
	check("GET /api/categories", catOK, fmt.Sprintf("status=%d 分类数=%d", code, catResp.Total))

	// 12. 库列表带分类字段
	code, body, _ = get("/api/libs")
	hasCat := strings.Contains(string(body), `"categoryKey"`)
	check("库列表含分类字段", code == 200 && hasCat, fmt.Sprintf("status=%d", code))

	// 12b. 库列表含文档版本字段（首页表格的「文档版本」列）
	hasLibVer := strings.Contains(string(body), `"libVersion"`)
	check("库列表含文档版本字段", code == 200 && hasLibVer, fmt.Sprintf("status=%d", code))

	// 12c. 库列表接口的响应头
	_, _, hdrLibs := get("/api/libs")
	check("接口响应不进缓存", headerHas(hdrLibs, "Cache-Control", "no-store"),
		"Cache-Control="+hdrLibs.Get("Cache-Control"))

	// 13. 融合检索：一次查询同时命中标题与正文
	t2 := time.Now()
	code, body, _ = get("/api/search?scope=title&q=" + urlEscape("接口"))
	firstSearch := time.Since(t2)
	var sr struct {
		Total  int               `json:"total"`
		TitleN int               `json:"titleN"`
		Items  []search.FusedHit `json:"items"`
		Page   struct {
			PageIndex int  `json:"pageIndex"`
			HasNext   bool `json:"hasNext"`
		} `json:"page"`
	}
	srOK := code == 200 && jsonUnmarshal(body, &sr) == nil && sr.Total > 0
	detail := fmt.Sprintf("status=%d total=%d 标题=%d 首次(含建索引)=%v",
		code, sr.Total, sr.TitleN, firstSearch.Round(time.Millisecond))
	if srOK && len(sr.Items) > 0 {
		detail += " 首条=" + truncStr(sr.Items[0].Title, 16)
	}
	check("GET /api/search 融合检索", srOK, detail)

	// 13a. 标题命中数必须是精确全量，不能被分页或上限掩盖
	titleCountOK := sr.TitleN > 0 && sr.TitleN >= len(sr.Items)
	check("标题命中总数精确（不受分页影响）", titleCountOK,
		fmt.Sprintf("titleN=%d 本页=%d", sr.TitleN, len(sr.Items)))

	// 13b. 融合检索：全文范围应同时出现标题命中与正文命中
	code, body, _ = get("/api/search?scope=all&q=" + urlEscape("接口") + "&firstLimit=30")
	var fr struct {
		Total   int               `json:"total"`
		TitleN  int               `json:"titleN"`
		Items   []search.FusedHit `json:"items"`
		Content struct {
			Enabled   bool `json:"enabled"`
			Scanned   int  `json:"scanned"`
			Matched   int  `json:"matched"`
			TotalLibs int  `json:"totalLibs"`
		} `json:"content"`
	}
	fuseOK := code == 200 && jsonUnmarshal(body, &fr) == nil
	hasTitle, hasContent := false, false
	for _, it := range fr.Items {
		if it.Source == search.SourceTitle {
			hasTitle = true
		}
		if it.Source == search.SourceContent {
			hasContent = true
		}
	}
	fuseDetail := fmt.Sprintf("status=%d total=%d 标题=%d 本页标题=%v 本页正文=%v 正文扫描=%d篇",
		code, fr.Total, fr.TitleN, hasTitle, hasContent, fr.Content.Scanned)
	// 第 1 页必须是标题与正文混排
	check("融合检索同时返回标题与正文命中", fuseOK && fr.TitleN > 0 && hasTitle && hasContent,
		fuseDetail)

	// 13c. 排序：标题命中必须排在正文命中之前
	orderOK := true
	seenContent := false
	for _, it := range fr.Items {
		if it.Source == search.SourceContent {
			seenContent = true
		} else if it.Source == search.SourceTitle && seenContent {
			orderOK = false // 正文命中之后又出现标题命中，次序错了
			break
		}
	}
	check("标题命中排在正文命中之前", orderOK,
		fmt.Sprintf("本页 %d 条，次序正确=%v", len(fr.Items), orderOK))

	// 13d. 热查询应很快（索引已建好）
	t3 := time.Now()
	code, body, _ = get("/api/search?scope=title&q=TCP")
	warm := time.Since(t3)
	hotOK := code == 200 && jsonUnmarshal(body, &sr) == nil && sr.Total > 0 && warm < 500*time.Millisecond
	check("融合检索热查询", hotOK,
		fmt.Sprintf("status=%d total=%d 耗时=%v", code, sr.Total, warm.Round(time.Millisecond)))

	// 13e. 排序：完全相等的标题应排最前
	_, body, _ = get("/api/search?scope=title&q=TCP")
	if jsonUnmarshal(body, &sr) == nil && len(sr.Items) > 0 {
		exactFirst := sr.Items[0].Title == "TCP" || sr.Items[0].Score == 0
		check("标题排序（精确优先）", exactFirst,
			fmt.Sprintf("首条=%q score=%d", truncStr(sr.Items[0].Title, 16), sr.Items[0].Score))
	} else {
		check("标题排序（精确优先）", false, "未取到结果")
	}

	// 14. 正文检索（低层通道 /api/grep，限定单库避免全库扫描太慢）
	t4 := time.Now()
	code, body, _ = get("/api/grep?lib=" + libs[0].LibID + "&limit=5&q=" + urlEscape("接口"))
	grepEl := time.Since(t4)
	var gr struct {
		Total   int `json:"total"`
		Scanned int `json:"scanned"`
		Matched int `json:"matched"`
		Items   []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	grepOK := code == 200 && jsonUnmarshal(body, &gr) == nil && gr.Scanned > 0
	gd := fmt.Sprintf("status=%d 扫描=%d 篇 命中=%d 耗时=%v", code, gr.Scanned, gr.Matched, grepEl.Round(time.Millisecond))
	if grepOK && len(gr.Items) > 0 {
		gd += " 首条=" + truncStr(gr.Items[0].Title, 14)
	}
	check("GET /api/grep 正文检索", grepOK, gd)

	// 14b. 正文检索必须能解出中文（验证 GBK 转码，这是最容易坏的一环）
	zhOK := false
	for _, it := range gr.Items {
		if strings.Contains(it.Snippet, "接口") || strings.Contains(it.Title, "接口") ||
			containsHan(it.Snippet) {
			zhOK = true
			break
		}
	}
	check("正文检索能解中文（GBK）", code == 200 && zhOK, fmt.Sprintf("中文摘要=%v", zhOK))

	// 14c. 检索词过短应被拒绝
	code, _, _ = get("/api/grep?q=" + urlEscape("的"))
	check("检索词过短被拒", code == 400, fmt.Sprintf("status=%d 期望 400", code))

	// 14d. 正文检索的领域过滤：带 cat 时命中的库都应属于该领域
	code, body, _ = get("/api/grep?q=" + urlEscape("接口") + "&cat=wireless&limit=20")
	if code == 200 {
		var gr struct {
			Items []struct {
				LibID string `json:"libId"`
			} `json:"items"`
		}
		catsOK := true
		if err := jsonUnmarshal(body, &gr); err == nil {
			wireless := map[string]bool{}
			for _, l := range libs {
				if doclib.CategoryOf(l).Key == "wireless" {
					wireless[l.LibID] = true
				}
			}
			for _, it := range gr.Items {
				if !wireless[it.LibID] {
					catsOK = false
					break
				}
			}
		}
		check("正文检索支持领域过滤", catsOK,
			fmt.Sprintf("命中 %d 条，全部属于 wireless=%v", len(gr.Items), catsOK))
	} else {
		check("正文检索支持领域过滤", false, fmt.Sprintf("status=%d", code))
	}

	// 14e. 融合检索的领域过滤：本页所有命中都应属于该领域
	code, body, _ = get("/api/search?scope=title&q=" + urlEscape("接口") + "&cat=wireless")
	if code == 200 {
		var fr struct {
			Items []struct {
				LibID string `json:"libId"`
			} `json:"items"`
		}
		catsOK := true
		if err := jsonUnmarshal(body, &fr); err == nil {
			wireless := map[string]bool{}
			for _, l := range libs {
				if doclib.CategoryOf(l).Key == "wireless" {
					wireless[l.LibID] = true
				}
			}
			for _, it := range fr.Items {
				if !wireless[it.LibID] {
					catsOK = false
					break
				}
			}
		}
		check("融合检索支持领域过滤", catsOK,
			fmt.Sprintf("命中 %d 条，全部属于 wireless=%v", len(fr.Items), catsOK))
	} else {
		check("融合检索支持领域过滤", false, fmt.Sprintf("status=%d", code))
	}

	// 14f. 分页：第 2 页与第 1 页的内容不重叠（确定性分页）
	//
	// 用 (nodeId,title,url) 作复合键：单看 title 会把不同文档误判为同一条，
	// 只看 nodeId 又会把「同一节点在不同包里的不同 URL」误判为重复。
	code, b1, _ := get("/api/search?scope=title&q=" + urlEscape("接口"))
	code2, b2, _ := get("/api/search?scope=title&q=" + urlEscape("接口") + "&page=2")
	// 再取一次第 1 页，验证同一查询的排序是确定的（无随机性）
	code3, b3, _ := get("/api/search?scope=title&q=" + urlEscape("接口"))
	if code == 200 && code2 == 200 && code3 == 200 {
		type item struct {
			NodeID string `json:"nodeId"`
			Title  string `json:"title"`
			URL    string `json:"url"`
		}
		var p1, p2, p1b struct {
			Items []item `json:"items"`
		}
		_ = jsonUnmarshal(b1, &p1)
		_ = jsonUnmarshal(b2, &p2)
		_ = jsonUnmarshal(b3, &p1b)
		k := func(it item) string { return it.NodeID + "|" + it.Title + "|" + it.URL }
		overlap := 0
		seen := map[string]bool{}
		for _, it := range p1.Items {
			seen[k(it)] = true
		}
		for _, it := range p2.Items {
			if seen[k(it)] {
				overlap++
			}
		}
		// 确定性：两次第 1 页必须逐条一致
		stable := len(p1.Items) == len(p1b.Items)
		if stable {
			for i := range p1.Items {
				if k(p1.Items[i]) != k(p1b.Items[i]) {
					stable = false
					break
				}
			}
		}
		check("分页无重叠（确定性分页）", overlap == 0 && len(p1.Items) > 0 && stable,
			fmt.Sprintf("第1页=%d 第2页=%d 重叠=%d 顺序稳定=%v", len(p1.Items), len(p2.Items), overlap, stable))
	} else {
		check("分页无重叠（确定性分页）", false, fmt.Sprintf("status=%d,%d,%d", code, code2, code3))
	}

	// 14g. 标题命中按主题合并：同一篇文档被多个包收录时只出一条，
	// 并通过 groups 列出它所在的全部文档包。
	code, body, _ = get("/api/search?scope=title&q=" + urlEscape("接口") + "&limit=100&firstLimit=100")
	if code == 200 {
		var fr struct {
			Items []struct {
				NodeID string `json:"nodeId"`
				Title  string `json:"title"`
				URL    string `json:"url"`
				Groups []struct {
					LibID   string `json:"libId"`
					LibName string `json:"libName"`
				} `json:"groups"`
			} `json:"items"`
		}
		uniqOK := true
		multi := 0
		if err := jsonUnmarshal(body, &fr); err == nil {
			seen := map[string]bool{}
			for _, it := range fr.Items {
				k := it.NodeID + "|" + it.Title + "|" + it.URL
				if seen[k] {
					uniqOK = false
					break
				}
				seen[k] = true
				if len(it.Groups) > 1 {
					multi++
				}
			}
		}
		check("标题命中按主题合并（同文档只出一条）", uniqOK && multi > 0,
			fmt.Sprintf("本页 %d 条，其中多包收录 %d 条，复合键唯一=%v", len(fr.Items), multi, uniqOK))
	} else {
		check("标题命中按主题合并（同文档只出一条）", false, fmt.Sprintf("status=%d", code))
	}

	// 14h. 限定单库检索（文档页「搜本文档」）：命中必须全部属于该库。
	//
	// 这是首页搜全库与文档页搜本文档的分界点：同一个接口，多一个 lib 参数。
	// 因此这里同时校验两件事：带 lib= 时结果里没有别的库，不带 lib= 时
	// 结果里确实有多个库（否则说明参数被忽略、退化成了单库）。
	{
		single := libs[0].LibID
		codeS, bodyS, _ := get("/api/search?q=" + urlEscape("接口") +
			"&lib=" + urlEscape(single) + "&limit=100&firstLimit=100")
		codeA, bodyA, _ := get("/api/search?q=" + urlEscape("接口") + "&limit=100&firstLimit=100")
		type scoped struct {
			LibScope string `json:"libScope"`
			Items    []struct {
				LibID string `json:"libId"`
			} `json:"items"`
		}
		var rs, ra scoped
		if codeS == 200 && codeA == 200 &&
			jsonUnmarshal(bodyS, &rs) == nil && jsonUnmarshal(bodyA, &ra) == nil {
			onlySingle := len(rs.Items) > 0 && rs.LibScope == single
			for _, it := range rs.Items {
				if it.LibID != single {
					onlySingle = false
					break
				}
			}
			multiInAll := map[string]bool{}
			for _, it := range ra.Items {
				multiInAll[it.LibID] = true
			}
			check("限定单库检索（文档页搜本文档）", onlySingle && len(multiInAll) > 1,
				fmt.Sprintf("单库=%s 命中 %d 条全属本库=%v；全库检索涉及 %d 个库",
					single, len(rs.Items), onlySingle, len(multiInAll)))
		} else {
			check("限定单库检索（文档页搜本文档）", false,
				fmt.Sprintf("status=%d,%d", codeS, codeA))
		}
	}

	// 14i. 单库检索时，正文段只扫这一个包（totalLibs=1）。
	// 范围收窄必须同时作用于标题段与正文段，否则会出现
	// 「标题只本文档、正文却来自全库」这种串档。
	{
		single := libs[0].LibID
		codeS, bodyS, _ := get("/api/search?q=" + urlEscape("接口") + "&lib=" + urlEscape(single))
		var rs struct {
			Content struct {
				Enabled    bool `json:"enabled"`
				TotalLibs  int  `json:"totalLibs"`
				ScannedLib int  `json:"scannedLib"`
			} `json:"content"`
		}
		if codeS == 200 && jsonUnmarshal(bodyS, &rs) == nil {
			check("单库检索时正文只扫本文档", rs.Content.Enabled && rs.Content.TotalLibs == 1,
				fmt.Sprintf("totalLibs=%d scannedLib=%d（应为 1）",
					rs.Content.TotalLibs, rs.Content.ScannedLib))
		} else {
			check("单库检索时正文只扫本文档", false, fmt.Sprintf("status=%d", codeS))
		}
	}

	// 15. 大目录解析（挑节点最多的包）
	bigLib := libs[0]
	for _, l := range libs {
		if l.TopicNumber > bigLib.TopicNumber {
			bigLib = l
		}
	}
	t1 := time.Now()
	code, _, _ = get("/api/nav?lib=" + bigLib.LibID + "&parent=")
	elapsed := time.Since(t1)
	check("大目录首次展开 "+truncStr(bigLib.LibName, 18), code == 200 && elapsed < 5*time.Second,
		fmt.Sprintf("status=%d 耗时=%v 节点数=%d", code, elapsed.Round(time.Millisecond), bigLib.TopicNumber))

	// 16. 正文语料库：建 -> 开 -> 扫 -> 取摘要，整条链路
	//
	// 只为最小的那个包建分片：全量建库约 4 分钟（82.8 万篇），不适合放进
	// 自检。这里验证的是链路本身：语料格式、分片读取、命中统计、
	// 按偏移取正文与元信息；全量覆盖能力由真实文档库上的手工验证覆盖。
	small := libs[0]
	for _, l := range libs {
		if l.TopicNumber > 0 && (small.TopicNumber == 0 || l.TopicNumber < small.TopicNumber) {
			small = l
		}
	}
	smallIdx := -1
	for i, l := range libs {
		if l.FilePath == small.FilePath {
			smallIdx = i
		}
	}
	cdir := filepath.Join(os.TempDir(), "web_ics-selftest-corpus")
	os.RemoveAll(cdir)
	defer os.RemoveAll(cdir)
	store := corpus.NewStore(cdir, libs)
	store.Refresh()
	tc := time.Now()
	st, cerr := store.BuildOne(smallIdx, small)
	check("建正文语料分片 "+truncStr(small.LibName, 18), cerr == nil && st.Docs > 0,
		fmt.Sprintf("%d 篇 / %.1f MB 耗时=%v", st.Docs, float64(st.TextBytes)/(1<<20),
			time.Since(tc).Round(time.Millisecond)))
	srv.SetCorpus(store)

	code, body, _ = get("/api/search?q=" + urlEscape("配置"))
	var cs struct {
		Total   int               `json:"total"`
		Items   []search.FusedHit `json:"items"`
		Content search.ContentDig `json:"content"`
		Corpus  struct {
			Enabled bool `json:"enabled"`
			Ready   int  `json:"ready"`
			Total   int  `json:"total"`
		} `json:"corpus"`
	}
	csOK := code == 200 && jsonUnmarshal(body, &cs) == nil
	// 只建了 1 个包的分片，所以：扫到 1 个包、范围内共 len(libs) 个、标记为未完成
	check("语料库检索只扫已建好的包",
		csOK && cs.Content.Enabled && cs.Content.ScannedLib == 1 &&
			cs.Content.TotalLibs == len(libs) && cs.Content.Incomplete,
		fmt.Sprintf("status=%d scannedLib=%d totalLibs=%d incomplete=%v",
			code, cs.Content.ScannedLib, cs.Content.TotalLibs, cs.Content.Incomplete))
	check("语料库状态如实上报",
		csOK && cs.Corpus.Enabled && cs.Corpus.Ready == 1 && cs.Corpus.Total == len(libs),
		fmt.Sprintf("ready=%d/%d", cs.Corpus.Ready, cs.Corpus.Total))
	// 默认每页 100 条、其中约 1/4 留给正文段，所以本页应当有正文命中；
	// 且它们必须全部来自那个已建好语料的包（不能串到没建索引的包上）。
	foreign, contentN := 0, 0
	for _, it := range cs.Items {
		if it.Source != search.SourceContent {
			continue
		}
		contentN++
		if it.LibID != small.LibID {
			foreign++
		}
	}
	check("语料命中全部来自已建索引的包",
		csOK && foreign == 0 && contentN > 0 && cs.Content.Matched > 0,
		fmt.Sprintf("正文命中=%d 越界=%d matched=%d", contentN, foreign, cs.Content.Matched))

	// 客户端证书认证：只验闸门的四条判断。
	//
	// 不起真 TLS 监听，也不现造证书：自检的价值在于「什么都没装也能跑」。
	// 这里直接构造带 TLS 状态的请求打 Handler。真实的握手、链校验与 EKU
	// 拦截由 internal/server 的单元测试覆盖。
	{
		leaf := &x509.Certificate{
			Subject:      pkix.Name{CommonName: "selftest"},
			SerialNumber: big.NewInt(1),
			Raw:          []byte("selftest-cert"),
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		verified := &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{leaf},
			VerifiedChains:   [][]*x509.Certificate{{leaf}},
		}
		// 带了证书但链没验过（VerifiedChains 为空）时同样要拒。
		unverified := &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{leaf},
		}

		authCfg := server.DefaultConfig()
		authCfg.DebugHealth = true
		authCfg.IPRate = 0

		status := func(deny string, state *tls.ConnectionState) int {
			ca, err := server.NewClientAuth(nil, deny)
			if err != nil {
				return -1
			}
			authCfg.ClientAuth = ca
			h := server.New(authCfg, index, nav.NewCache(), nil).Handler()
			req := httptest.NewRequest(http.MethodGet, "/api/libs", nil)
			req.TLS = state
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Code
		}

		check("客户端证书：不带证书被拒", status("", nil) == http.StatusUnauthorized, "")
		check("客户端证书：链未校验时被拒", status("", unverified) == http.StatusUnauthorized, "")
		check("客户端证书：带有效证书放行", status("", verified) == http.StatusOK, "")
		check("客户端证书：吊销名单命中被拒",
			status("sha256:"+server.Fingerprint(leaf)+"\n", verified) == http.StatusForbidden, "")
	}

	fmt.Println()
	if fail == 0 {
		fmt.Println("=== 自检通过 ===")
		return 0
	}
	fmt.Printf("=== 自检失败：%d 项 ===\n", fail)
	return 1
}

// post 发一个 POST 请求，用于验证方法限制。
func post(base, path string) (int, []byte, http.Header) {
	resp, err := http.Post(base+path, "text/plain", strings.NewReader("x"))
	if err != nil {
		return 0, nil, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, resp.Header
}

func isDocHTML(u string) bool {
	u = strings.ToLower(strings.Split(u, "#")[0])
	return strings.HasSuffix(u, ".html") || strings.HasSuffix(u, ".htm")
}

// urlEscape 对查询参数做 URL 编码（中文必须编码，否则请求行非法）。
func urlEscape(s string) string { return url.QueryEscape(s) }

// headerHas 判断响应头 key 的值里是否包含子串（大小写不敏感的关键字匹配）。
func headerHas(h http.Header, key, want string) bool {
	return strings.Contains(strings.ToLower(h.Get(key)), strings.ToLower(want))
}

// containsHan 判断字符串里是否含汉字。
//
// 用于验证「正文检索是否真的按 GBK 解码成功」：若解码失败，
// 摘要里会是乱码或纯 ASCII，此时应判定为失败。
func containsHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// printEnv 打印环境信息，便于排查部署问题。
func printEnv(docRoot string, cf *configFile) {
	wd, _ := os.Getwd()
	fmt.Printf("工作目录: %s\n", wd)
	fmt.Printf("配置文件: %s\n", cf.describe())
	fmt.Printf("文档库:   %s (存在=%v)\n", docRoot, dirExists(docRoot))
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// truncate 截断过长字符串，只用于自检输出，避免刷屏。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

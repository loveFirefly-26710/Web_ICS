# Web_ICS 开发文档

本文件面向开发者、维护者与代码评审：包职责、模块设计、数据流、接口参考、
开发规范、测试与调试方法、部署与发布流程。

---

## 文档定位

| 文档 | 读者 | 内容范围 | 维护职责 |
|---|---|---|---|
| [README.md](README.md) | 使用者、部署者、运维 | 是什么、怎么装、怎么跑、怎么排障、资源占用 | 面向用户的行为变了就改：命令行参数、默认值、界面交互、部署步骤、实测指标 |
| DEVELOPMENT.md（本文） | 开发者、维护者、代码评审 | 术语表、包职责、模块设计、数据流、接口参考、开发规范、测试与调试、发布流程 | 面向开发者的结构变了就改：包职责、函数签名、接口字段、构建与发布步骤、排查手法 |
| [PLAN.md](PLAN.md) | 架构与技术负责人、后续开发者 | 设计目标、调研结论、内存预算、决策记录、里程碑、风险、后续规划与待办 | 做完一轮设计或规划就改：新决策、已完成的里程碑、新增待办、风险状态 |

改动代码时按这一条自检：这次改动会让哪一份文档变成错的？通常不止一份。

性能与资源数字的权威出处是 [README.md 的实测数据](README.md#实测数据)，本文不重复。
设计取舍的原因记在 [PLAN.md](PLAN.md)，本文只写结论与约束。

---

## 术语表

全项目统一使用下列术语。三份文档、代码注释、提交信息都以此为准。

| 术语 | 英文 / 标识符 | 含义 |
|---|---|---|
| 文档包 | package / `.hdx` / `.hwics` | 单个产品文档压缩包，本质是标准 ZIP |
| 文档库 | doc root | 存放文档包的目录，即 `--doc-root`。扫描只读一层，不递归 |
| 库名 | `LibName` | 文档包的展示名，来自 `profile.xml` 的 `libName`，缺失时用文件名 |
| 库标识 | `LibID` | 文档包标识，来自 `profile.xml` 的 `libId` 或 `libid`（两套布局，见「包内 profile.xml」），缺失时由文件名推导。它不唯一，本机有 4 组同名同 libID 的不同发行版 |
| 产品领域 | `Category` | 无线 / 数据通信 / 计算 等大类，由规则表从库名判定 |
| 目录树 | tree / nav | 从包内 `navi.xml`（优先）或 `.hhc` 解析出的层级结构 |
| 目录节点 | `Node` | 目录树的一个节点，字段 `ID` / `Parent` / `Title` / `URL` / `HasKid` |
| 深链 | deep link | 阅读页 URL 上的 `?url=` / `?node=` 参数，用于把目录树展开并选中到某一篇 |
| 首页 | home | `/` 路由返回的外壳页面 |
| 阅读页 | viewer | `/doc/{libId}/` 路由返回的外壳页面（`web/viewer.html`） |
| 裸文档页 | raw topic | 包内原始 HTML，没有顶栏与目录树。搜索结果不能落在它上面 |
| 正文 | topic / resource | 包内 `resources/` 下的静态资源（HTML / 图片 / CSS / JS / PDF） |
| 标题索引 | `TitleIndex` | 常驻内存的列式标题索引，扫全部包的目录标题建成 |
| 标题段 | title segment | 融合检索结果里来自标题索引的那一段（`Score = 0`） |
| 正文段 | content segment | 融合检索结果里来自正文的那一段（`Score = 1`） |
| 融合检索 | fused search | 一次查询同时打标题与正文，是唯一的检索链路 |
| 语料库 | corpus | 离线抽取的磁盘纯文本索引，供正文段全库扫描 |
| 语料分片 | shard | 语料库的最小单位，每个文档包一个 `.icsc` 文件 |
| 内存守卫 | `memguard` | 周期性检查 RSS 并执行降级动作的组件 |
| 抽取版本 | `ExtractRevision` | `internal/htmltext` 里的常量，抽取逻辑一改就必须加一 |
| 格式版本 | `formatVer` | 语料分片格式版本，当前为 2 |
| 采集上限 | `CollectLimit` | 单次检索的内存收集上限，不是结果上限 |
| 扫描覆盖 | `Scanned` / `Incomplete` | 扫描进度信息，不是「结果被截断」 |

措辞约定：`total` 一律指全量命中数，不是「本页取回多少条」；
`Incomplete` / `MoreLibs` 一律指「扫描尚未覆盖全部范围」，
界面文案必须写成「已就绪 X/N」或「已扫描 X/Y 库」，不要写「结果已截断」。

「搜索」指用户动作与界面文案（搜索框、搜索结果、搜索行为）；
「检索」指机制与接口（融合检索、正文检索、检索链路）。两者不混用。

---

## 技术栈与设计约束

| 层 | 选择 | 理由 |
|---|---|---|
| 语言 | Go 1.25+，`CGO_ENABLED=0` | 静态编译无依赖，交叉编译一条命令，内存可控 |
| HTTP | 标准库 `net/http` | 够用，零依赖 |
| 解包 | 标准库 `archive/zip` | `ReaderAt` 随机访问，不落盘解压 |
| 依赖 | `golang.org/x/text v0.21.0` | 唯一第三方依赖，用于 GBK 解码 |
| 前端 | 原生 HTML / CSS / JS | 不引框架，省掉 jQuery / Bootstrap / zTree 几百 KB |
| 检索 | 标题走常驻列式索引，正文走磁盘语料库全量扫 | 见 [PLAN.md 的内存预算](PLAN.md#内存预算) |

三条硬约束，改动前先确认没有违反：

1. 进程 RSS 不超过 1 GB（硬约束），设计目标 400 MB 以内。
   守卫阈值是软 350 / 告警 450 / 硬 500，都高于正常运行区间，只在异常时触发。
2. 不整文件读入内存。正文、图片、PDF 一律 `io.Copy` 流式；
   只有元数据类小文件走 `ReadAllBounded`，且有显式上限。
3. 文档包只读。服务没有任何写接口，也不写文档库目录。
   唯一写盘的地方是语料目录。

---

## 目录结构与包职责

```
cmd/web_ics/          入口、命令行解析、内建自检
internal/doclib/      文档包索引与 zip 流式读取
internal/nav/         目录树解析 + 单包 LRU + 深链路径
internal/htmltext/    HTML 转纯文本（GBK 前置解码）
internal/textfold/    ASCII 大小写折叠，字节长度严格不变
internal/search/      标题索引与融合检索
internal/corpus/      正文语料库：分片格式、建索引、全库扫描
internal/server/      HTTP 服务、路由、并发闸门、限流、安全头、客户端证书认证
internal/memguard/    内存自守与 Go 运行时内存上限
web/                  前端（原生 HTML/CSS/JS），同时是把它 embed 进二进制的 Go 包
docs/                 正文语料库（默认位置）
dist/                 构建产物（不进版本库）
```

| 包 | 职责 | 不做什么 |
|---|---|---|
| `cmd/web_ics` | 解析命令行与环境变量、设置运行时参数、装配各组件、优雅退出、内建自检 | 不含业务逻辑。`selftest.go` 只做端到端断言 |
| `internal/doclib` | 扫描文档库目录、读 `profile.xml` 建库索引、按需打开 zip、大小写不敏感查找、产品领域规则表 | 不解析目录树（那是 `nav`），不缓存内容 |
| `internal/nav` | 解析 `navi.xml` / `.hhc` / `FileList.xml` 成扁平树、单包 LRU 缓存、按层取子节点、深链祖先链 | 不关心正文，不做全库缓存 |
| `internal/htmltext` | 剥标签、还原实体、GBK 转 UTF-8、抽标题、判定噪声路径 | 不做完整 HTML 解析，够用即可 |
| `internal/textfold` | 只折叠 ASCII 字母且字节长度不变 | 不做非 ASCII 大小写折叠 |
| `internal/search` | 标题索引的构建、释放与查询，融合检索，正文段两条路径，摘要生成 | 不直接解压 zip，依赖由 `Options` 注入 |
| `internal/corpus` | 分片格式与读写、建分片、新鲜度判定、全库并行扫描、按 URL 反查文档 | 不做分页与摘要（那是 `search`） |
| `internal/server` | 路由、中间件链、并发闸门、每 IP 限流、安全头、客户端证书认证、Content-Type 处理、JSON 响应 | 不含检索算法 |
| `internal/memguard` | 周期性采样 RSS、四档降级、Go 运行时内存上限、对外粗粒度指标 | 不决定释放什么，由 `main` 注入回调 |

`internal/search` 与 `internal/corpus` 的文件分工：

```
search/index.go          标题索引：构建 831,354 条、列式存储加字符串池去重、刷新状态机
search/fused.go          融合检索：一次查询同时打标题（Score 0）与正文（Score 1）、分页
search/corpuscontent.go  正文段走语料库的路径（全库扫描，精确 total），并给标题命中补摘要
search/grep.go           正文段兜底路径：逐包流式扫描加摘要与高亮

corpus/shard.go          分片格式（textBlob + offsets + metaBlob + urlIdx）与建分片
corpus/scan.go           全库扫描：分块加 ASCII 折叠查找加工作缓冲池化
corpus/store.go          分片生命周期：新鲜度检查、后台增量重建、并行查询
```

---

## 核心架构

```
浏览器（原生 JS，无框架）
  ├─ 首页  web/index.html + home.js
  └─ 阅读页 web/viewer.html + viewer.js
        ├─ 目录树：懒加载，展开时才请求该层
        └─ iframe：正文 HTML，sandbox="allow-scripts"
              │  HTTP :8080
┌─────────────▼───────────────────────────────────────────────┐
│ Go 单进程 web_ics                                            │
│  中间件链（由外到内）：                                       │
│    withSecurityHeaders → withMethodGuard                     │
│      → withRateLimit → withGate → mux                        │
│                                                              │
│  路由：                                                       │
│    /                       首页外壳                          │
│    /doc/{libId}/           阅读页外壳                        │
│    /doc/{libId}/res/*      正文、图片、PDF 流式直出          │
│    /api/libs              库元数据                           │
│    /api/categories        产品领域分类                       │
│    /api/nav               目录树按层 / 深链祖先链            │
│    /api/search            融合检索（界面入口）               │
│    /api/grep              底层正文检索通道                   │
│    /healthz               粗粒度健康检查                     │
│    /debug/healthz         详细指标（默认关闭）               │
│                                                              │
│  内存里的东西（只有这些是常驻的）：                           │
│    ├─ 库索引 59 条元数据          约 100 KB                  │
│    ├─ 标题索引 831,354 条          峰值后约 290 MB（可释放） │
│    ├─ 目录树缓存（容量 1 个包）    最大包约 43 MB（可清空）   │
│    └─ 语料分片的 offsets 表        每包约 144 KB（随查询开合）│
│                                                              │
│  内存守卫（每 5 s 采样）：软限清缓存，告警线降级，硬限计数     │
└──────────────┬───────────────────────────────────────────────┘
               │
   ┌───────────┴────────────┐
   │ 文档库（只读，11.5 GiB）│  zip 直读，零额外磁盘
   │ 语料库（可写，1.73 GiB）│  离线抽取的纯文本
   └────────────────────────┘
```

只有四样东西常驻内存，这是全部内存策略的落脚点。任何一个新功能如果要往内存里放
随文档规模增长的东西，都必须先算这笔账，并纳入内存守卫的释放清单。

---

## 模块设计

### internal/doclib — 文档包索引与读取

index.go：

- `LibMeta` 是库元数据（`LibID` / `LibName` / `ProductType` / `ProductVersion` /
  `LibVersion` / `Language` / `IssueDate` / `TopicNumber` / `Guid` /
  `FilePath` / `FileName` / `SizeBytes` / `HasNaviXML`）。
- `Scan(root)` 遍历 `root` 的直接子文件，取 `.hdx` / `.hwics`，逐个打开 zip 只读
  `profile.xml`（包根或 `resources/infocenter_service/` 下，见「包内 profile.xml」
  一节，`io.LimitReader` 上限 1 MiB）并探测 `resources/navi.xml` 是否存在。
  单包读失败不中断整体扫描，退化成只用文件名的条目。结果按 `LibName` 排序。
  内存上每包约 1 KB，59 包合计不到 100 KB。
- `libIDFromName` 在没有 GUID 时从文件名推导稳定 ID：保留字母数字与 CJK（`r > 0x7f`），
  其余折叠成单个 `-`，首尾去 `-`。
- `Index` 的 `List()` 与 `Get(libID)` 都返回副本，内部用 `RWMutex` 保护。

reader.go：

- `Reader` 包一层 `zip.ReadCloser`，并提供惰性大小写折叠索引。`exact` 是原始路径到
  `*zip.File`，`folded` 是小写路径到 `*zip.File`。`Lookup(name)` 先查 `exact`，
  未命中再查 `folded`。构建一次约 10~30 ms（3.6 万条目），之后查询 O(1)。
  不用线性扫描，因为大包每次请求扫 3.6 万条会明显拖慢正文响应。
- 这条回退不是可选容错，是复现文档自身引用行为的必要条件：目录 URL 26.4%、
  正文内部引用 35.5% 只能靠忽略大小写命中。漏掉它会出现两类看似无关的故障：
  图片和脚本 404，以及正文标签页「点不动」（`customQuery.js` 与 `tabSection.js` 双双
  404，导致 `showCurrent()` 未定义，正文里内联的 `onclick` 静默失败）。
- `ReadAllBounded(name, limit)` 只用于小文件（元数据、`navi.xml`），有显式上限。
- `Stream(name, w)` 是正文、图片、PDF 的唯一读取路径，走 `io.Copy`，
  内存占用与文件大小无关。
- `normalizeZipPath` 把反斜杠转成正斜杠、去前导斜杠、`path.Clean`，并拒绝 `../` 穿越。

category.go：

- `categoryRules` 是顺序敏感的规则表（11 条加兜底 `other`）。按切片顺序匹配，先中者胜，
  因此更具体的规则必须排在前面。例：SD-WAN 属于解决方案而非具体产品，
  所以排在数据通信之前。
- `CategoryOf(meta)` 先匹配 `LibName`，再匹配 `FileName`，不匹配 `productType`
  （部分包的 `productType` 是版本串或空值）。
- `Index.Categories()` 只返回实际出现过的分类并填好 `Count`，
  因为界面上不该出现一个点进去什么都没有的导航项。
  加一个领域只需改这张规则表，前端不硬编码分类。

### internal/nav — 目录树

`Parse` 的解析优先级：

| 优先级 | 来源 | 做法 |
|---|---|---|
| 1 | `resources/navi.xml` | `xml.Decoder` 流式解析 `<topic txt url id>` 嵌套，不 Unmarshal 整棵树 |
| 2 | `resources/*.hhc` | 手工扫描 CHM sitemap，GBK 转码后解析 |
| 3 | `resources/FileList.xml` | 平铺无层级，仅兜底 |

- `maxMetaBytes = 32 MiB` 是包内元数据文件的读取上限。实际最大的 `.hhc` 是 10.54 MiB，
  留 3 倍余量。树要常驻，上限给太大等于给常驻内存留口子。
- `.hhc` 的两个坑都在 `hhc.go` 里钉死了：
  - 按 `</LI>` 弹栈，不是 `</UL>`。叶节点没有 `<UL>`，全库样本里 `<LI>` 有 55,664 个
    而 `<UL>` 只有 6,724 个，按 `</UL>` 弹会让整棵树塌成单根。
  - 不做 O(n²) 的 `ToLower`。只做一次小写副本，之后基于游标做 `Index`。
    否则 10 MiB 的 `.hhc` 会跑到分钟级（实测从 10 分钟超时降到 382 ms）。

`Tree` 是扁平存储加索引：

```go
type Tree struct {
    Nodes    []Node
    Children map[string][]int // parentID -> 子节点下标
    Roots    []int
    byID     map[string]int
}
```

- `ChildrenOf(parent)` 按层取直接子节点，`parent` 为空返回顶层。
- `Path(id)` 与 `PathByURL(url)` 返回「根到目标」的节点链，供深链定位。
  树是扁平的，顺 `Parent` 走回根再反转，带 `guard < 4096` 防自环挂死。
  `PathByURL` 线性扫一遍，只在深链请求时调用，不必额外建索引常驻内存。

`Cache` 是容量为 1 的单包目录树缓存。用户同一时间只读一个库，容量 1 时内存上限
就是单个最大包的树（约 43 MB），而全量缓存 59 包要 867 MB。`Get` 在解析期间不持锁，
避免阻塞其它库。`Drop()` 由内存守卫调用。

### internal/htmltext — HTML 抽文本

- `ExtractRevision = 2` 是抽取逻辑的版本号。任何会改变抽取输出的改动都必须把它加一，
  否则已建好的语料会被判定为就绪而不重建，症状是「明明改了代码、结果一点没变」，
  而且不报任何错。
- `ExtractText(raw)` 先 `DecodeToUTF8`，再逐字节扫描：整体跳过 `<script>` 与 `<style>`，
  其它标签替换为空格（避免「端口地址」这类粘连），还原常见实体与数字实体，
  最后用 `strings.Fields` 折叠空白。末尾剔除文档模板的顶部导航条，
  它渲染出来是 `< Home`，既污染摘要又造成「搜 Home 命中所有文档」的假命中。
- `ExtractTitle(raw)` 优先取 `<title>`，其次取 `class="topicTitle-h1"` 的 `h1`。
  同样必须先做 GBK 转码，`ExtractTitle` 漏掉这一步会导致搜索结果标题一直是空的。
- `DecodeToUTF8(raw)` 先做 `utf8.Valid` 判断（已是 UTF-8 就直接返回，省一次转码），
  否则走 `simplifiedchinese.GBK.NewDecoder()`，失败退回原字节。
- `IsNoisePath(lower)` 判定导航类页面（`/index.html`、`hedex-homepage`、`copyright`、
  `glossary`、`search.html`）。`search` 与 `corpus` 共用这一份判定，
  两边各写一份必然漂移。
- 这个包被单独拆出来，是因为 `search` 与 `corpus` 都要用同一套抽取逻辑。
  抽成独立包两边 import 它，也就不会出现循环依赖。

### internal/textfold — 字节长度不变的大小写折叠

`ASCIILower(s)` 只把 `A-Z` 转成 `a-z`，返回值的字节长度与输入严格相等。

不能用 `strings.ToLower` 的原因：它遇到非法 UTF-8 字节会把每个非法字节替换成
`U+FFFD`（3 字节），输出比输入长。代码里常见的写法

```go
lower := strings.ToLower(s)
idx := strings.Index(lower, needle)
rest := s[idx:]          // 越界 panic
```

拿「在折叠串上算出的下标」去切原串就会 `slice bounds out of range`。
本仓库有四处这样的写法（`nav/hhc.go` 的 `paramValue`、`htmltext/extract.go` 的
`betweenTag` 与 `skipTo`、`search/grep.go` 的 `snippets`），都必须用 `textfold`。
`IndexFold(s, sub)` 是配套的便捷函数，返回的下标对 `s` 本身同样有效。

### internal/search — 标题索引与融合检索

#### TitleIndex（index.go）

列式存储，不是 `[]struct`：

```go
type titleCols struct {
    n       int
    titles  []string // 标题
    libIDs  []uint16 // 指向 libList 的下标（库数量有限，uint16 足够）
    urls    []string
    nodeID  []string
    parID   []string
    breadth []string // 章节路径（祖先标题用 " / " 连接）
    libList []string
}
```

列式的收益是决定性的。`[]titleRow` 每行 6 个 string、96 字节，扫描要跨几十万行做
指针追逐，缓存命中率极低；列式把 `titles` 连续存放，扫描只触碰这一份紧凑数组
（实测查询延迟从 140~190 ms 降到 20~35 ms）。附带好处是 GC 只需扫 8 个切片对象，
而不是 83 万个对象乘 6 个指针。

`stringPool.intern` 做字符串去重。`libID` 只有几十种、`breadth` 重复率极高，
不去重时这些字符串会被复制几十万份（内存从 252 MB 降到 98 MB）。

单包解析有三条上限，一起把畸形文档包的最坏内存占用钉死：
`maxPathDepth = 64`、`maxPathBytes = 16 MiB`、`maxNodesPerLib = 200000`。
章节路径是祖先标题的线性连接，深度 N 会产生 O(N²) 的字符串总量；
一个 36 MiB、100 万层的畸形 `navi.xml` 能把单次搜索的 RSS 从 1.3 GB 推到 7.7 GB。

#### 刷新状态机（并发核心）

索引是懒构建的（首次搜索时才扫全部 `navi.xml`），并且可被内存守卫释放后重建。
这带来一组并发要求：构建要数秒，释放可能随时发生，两者不能互相饿死。

```go
type TitleIndex struct {
    buildMu   sync.Mutex   // 串行化「挂载 / 释放」
    refreshMu sync.Mutex   // 串行化「构建」本身
    refresh   atomic.Int32 // refreshIdle / Running / Done / Dead
    mu        sync.RWMutex // 只保护 cols 指针的读写
    cols      *titleCols
}
```

锁序固定为 `refreshMu → buildMu → mu`，绝不反向。

不能用 `sync.Once`：`Once.Do` 在回调返回前会阻塞其他所有调用者，而回调要数秒，
中间还夹着 `Release()`。「进入后不许他人插队」与「内存压力下随时要能放弃」是冲突的。
强行在 `Release` 里复位 `Once` 会触发
`fatal error: sync: unlock of unlocked mutex`，Go runtime 直接 abort，
`recover` 抓不住。

`buildMu` 与 `refreshMu` 要分开的原因：构建是长操作，若一直持 `buildMu`，
内存守卫的 `Release` 会被卡住整整一次构建时长，而守卫是每 5 秒一次，
结果是守卫排队、内存也救不下来。分开后 `Release` 只在挂载与卸载的瞬间拿 `buildMu`，
耗时是微秒级。

`refreshDead` 分支里的 `fallthrough` 不能省。CAS 失败后必须往下走去抢构建权，
否则所有人都在「把 idle CAS 成 running，有人成功、其余人失败」之间打转，
goroutine 一直活着却什么都没做。这是进程挂起，不是崩溃。

`buildGrace = 10 s` 让 `Release` 遇到在途构建时先给宽限窗口，不当场作废。
否则会出现「构建烧掉几秒、出锁发现被作废、结果被丢弃、立刻从头再来」的循环，
查询线程一直构建一直被打断，用户看到的只是「检索中…」。

`maxBuildAttempts = 3` 限制构建被作废后的重试次数，避免内存压力持续时无限重试。

#### Fused（fused.go）

- 一条链路。`keep` 是合成好的「这个库要不要算」判断（领域过滤与单库范围），
  标题段、正文段的库清单、正文命中输出、进度提示四处全用它。
  漏掉任何一处就会出现「标题只本文档、正文却来自全库」的串档。
- 排序用整数分级表达权重 10:1：标题命中 `Score = 0` 整体在前，正文命中 `Score = 1` 在后。
- `offset` 把两段看成一条连续结果流 `[标题 0..T-1][正文 0..C-1]`，
  第 1 页取前 `FirstPageSize` 条，第 k 页取接下来的 `PageSize` 条。
- `contentFirstPageShare = 4` 让第 1 页让出约 1/4 给正文段。否则标题命中吃满首屏，
  用户在「全文」范围下会看到清一色标题结果，误以为没有正文命中。
- `contentProbe = 20`：第 1 页被标题占满时仍做一次限量正文探测，
  用来算 `total` 与判断 `HasNext`，让用户知道翻页还有正文结果。
- `searchTitlesAll` 的合并键是 `(title, url)`，不是 `title`。`url` 才是文档身份，
  按 `title` 合并会把不同文档的同名标题误并。组内再按 `(libName, url)` 去重，
  因为同名不同 libID 的包显示名一样，列两次是噪音。

#### 正文段的两条路径（corpuscontent.go / grep.go）

| 路径 | 触发条件 | 覆盖 | 代价 |
|---|---|---|---|
| 语料库（`corpusContent`） | 语料库至少建好 1 个包 | 全部已建好的包 | 0.6~1.3 s |
| 逐包兜底（`fusedContent` 加 `Grep`） | 语料库一个包都没建好 | 一次请求推进一个包 | 每包数秒 |

兜底路径存在的唯一目的是让「语料没建完时搜索不会完全不可用」。它的语义与语料库路径
刻意保持一致，但 `Matched` 的含义会退化成「扫到停手为止已确认的命中篇数」。
`WantsExactCount = false` 时不同页的 `Matched` 不再相等，这是被主动放弃的性质。

摘要生成（`snippets`）的要点：

- `ctxLen = 60`，命中词用 `<em>` 包裹，转义在拼接前逐段做。不要先拼成含占位符的整串
  再整体转义，因为正文里恰好出现该占位符时会被替换成 `<em>`，
  等于让文档内容往结果里注入标记。
- `alignRune` 把下标对齐到 rune 起点，否则会把一个汉字劈成半个、输出乱码。
- 记录上一段的结束位置，新段起点没越过它就不收。短文档里相邻两次命中会产出两段
  几乎一样的文字，卡片上看起来像重复渲染。这步不影响命中次数统计。
- `ContentMaxDoc` 的默认值必须在调用方兜。`snippets(text, q, 0)` 的循环一次都不执行，
  摘要为空、条目被 `continue` 丢掉，表现是「正文命中数算得完全正确，
  但本页一条正文都看不到」。`corpusContent` 里兜成 3。

### internal/corpus — 磁盘语料库

设计结论是不建倒排索引。全库正文纯文本只有 1.61 GiB，`strings.Index` 实测吞吐
1.7~6 GB/s，全量扫一遍只要 0.25~0.9 s。倒排索引要 3~9 GB 还得处理中文 n-gram 切分，
而全扫天然精确、天然覆盖全库、天然确定性分页，代码量还少一个数量级。

#### 分片格式 v2（shard.go，headerSize = 64）

```
 0   魔数 "WICS" + 格式版本(1B) + 抽取版本(1B)      <- 第 5 字节是 htmltext.ExtractRevision
 8   docCount u32
12   metaBlobLen u32
16   textBlobLen u64
24   textOff u64 / offsetsOff u64 / metaOffsetsOff u64 / metaBlobOff u64
56   urlIdxOff u64
64   textBlob      每篇正文 + '\n'（分隔符防止跨篇误匹配）
     offsets       docCount × u32，正文起点（升序）
     metaOffsets   docCount × u32，指向 metaBlob
     metaBlob      每篇 [u16 urlLen][url][u16 titleLen][title][u16 breadthLen][breadth]
     urlIdx        docCount × [u64 urlHash][u32 docIdx]，按 hash 升序
```

- 只有 `offsets` 常驻内存（3.6 万篇 = 144 KB），其余按需 `ReadAt`。
- 不压缩是刻意的。压缩后随机读一篇要解压整块；顺序扫 1.61 GiB 在任何磁盘上都不慢，
  解压反而会变成新瓶颈。
- `metaBlob` 里的 `breadth` 是章节路径，建分片时从目录树带进来。正文命中的搜索结果
  只有文档本身、没有节点信息，拿不到「命令参考 > 安全 > PKI配置命令」这一行。
  在这里存下来，查询时零额外开销。分隔符与标题索引保持一致，都是 `" / "`。
- `urlIdx` 供「标题命中也要配正文摘要」用。标题索引给的是 `(libID, url)`，
  而正文按文档序号存，必须能按 URL 反查。用排序数组加二分加 `ReadAt`，
  不要把整张表读进内存（59 个分片全常驻要几十 MB，而二分只要十几次 `ReadAt`）。
- `normalizeURLKey` 去空白、去 `#` 锚点、去 `./` 与 `resources/` 前缀、转小写。
  必须归一化，因为目录里的 URL 大小写常与包内文件名不一致（26.4%），
  不归一化就查不到。

#### ScanInto（scan.go）

- `scanChunk = 1 MiB`、`ctxKeep = 512`，块间保留尾部上下文，保证跨块命中不漏。
- 峰值内存与包大小无关。`scanScratch`（读缓冲加逐篇计数表）用 `sync.Pool` 复用，
  异常大的缓冲不留在池里。不池化时一次查询要扫几十个分片、每片一个读缓冲，
  短命对象会把 RSS 顶到内存软限附近。
- 命中直接追加进调用方的紧凑切片（`RawHit`，12 字节），不要先产出一层 `[]DocHit`
  再转换。常见词每包几万篇，中间那层就是每查询几十 MB 的垃圾。
- 必须把 `nextScan` 夹到 0。`nextScan -= keepFrom` 在「命中在块开头、
  块尾离它还有几 MB」时会变负，下一轮 `bytes.Index(buf[from:], ...)` 直接
  `panic: slice bounds out of range`，服务当场挂掉。`indexFold` 里也加了
  `if from < 0 { from = 0 }` 兜一层。
- `indexFold` 的锚点策略：检索词没有 ASCII 字母（纯中文）就直接 `bytes.Index`（SIMD）；
  有 ASCII 字母时挑第一个 ASCII 字母当锚点，用 `IndexByte` 找大小写两种形式，
  命中后逐字节折叠校验整个检索词。锚点选 ASCII 字母很关键，因为中文正文里 ASCII 稀疏、
  候选极少，而锚点落在中文字节（0xE6 这类）上候选会密集到退化。
  绝不对整段做 `ToLower`：对 222 MB 文本实测要 1.4 s，外推全库 1.61 GiB 就是 9 s，
  比扫描本身慢一个数量级。

#### Store（store.go）

- 分片按文档包而不是按 libID 命名，格式是
  `safeName(libID) + "-" + CRC32(FileName) + ".icsc"`。本机有 4 组同名同 libID 的
  不同发行版，用 libID 当文件名会让后建的覆盖先建的（实测 59 个包只落成 55 个分片），
  而缺失检查还认为都就绪，静默漏掉 4 个包的正文。`BuildAll` 的待建列表也按包文件路径
  映射回下标，不能用 libID 认包。
- `shardFresh(shard, pkg)` 有三个判据，缺一不可：

  | 判据 | 漏掉它的后果 |
  |---|---|
  | mtime（分片比包新） | 文档包换了内容不重建 |
  | 格式版本（`formatVer`） | 格式升级后旧分片仍判为就绪，而 `OpenShard` 又因版本不符失败，那些包的正文静默从检索里消失（覆盖数还显示 59/59） |
  | 抽取版本（`ExtractRevision`） | 改了抽取逻辑，分片仍判为就绪、根本不重建，「明明改了代码，结果一点没变」，同样不报错 |

- `Query(keep, q)` 并行扫分片，worker 数是 `min(GOMAXPROCS, 4)`。每片扫完保持打开
  （本页摘要要读它的正文），响应结束后由调用方 `Close`。排序规则是命中次数多的在前，
  其次按包、按文档序号，完全确定，因此翻页不串页、同一查询多次请求逐条一致。
- `BuildShard` 流式写盘（逐篇抽取直接写 `textBlob`），最后 `WriteAt` 回填头部再
  `Rename`。最大的包正文可达 70 MB，攒在内存里会顶到软限。

### internal/server — HTTP 服务

中间件链的顺序有讲究：

```go
// 没配客户端证书认证时
withSecurityHeaders(withMethodGuard(s.withRateLimit(s.withGate(mux))))

// 配了之后（ca 是 *ClientAuth），闸门插在安全头里面、方法判断外面
withSecurityHeaders(ca.withClientCertAuth(withMethodGuard(s.withRateLimit(s.withGate(mux)))))
```

- `withSecurityHeaders` 放最外层，这样限流的 `429`、闸门的 `503`、方法拒绝的 `405`、
  以及客户端证书的 `401` 与 `403` 也带上头。
- `withClientCertAuth` 排在方法判断与限流外面，未认证的请求不消耗并发槽位与限流
  配额。它只在配了 `client-ca` 时存在，见下面「客户端证书认证」一节。
- `withMethodGuard` 在限流之前，非 GET 与 HEAD 请求不消耗限流配额。服务是只读的，
  显式拒绝而不是静默按 GET 处理，可以避免请求体被中间层当成有效载荷缓存或转发。
- `withRateLimit` 只作用于 `/api/search` 与 `/api/grep`（`isExpensivePath`）。
  不限 `/api/nav` 与 `/static`：它们便宜，而且深链展开一棵几十层的目录树本来就要
  连发几十个 `/api/nav`，统一限流会把正常浏览挡掉。
- `withGate` 在内存降级时返回 `503`（带 `Retry-After: 5`），全局信号量满时返回 `503`
  （带 `Retry-After: 2`）。

#### 客户端证书认证（`auth.go`）

`ClientAuth` 只在配了 `--client-ca` 时构造，为 `nil` 时整条链路与加这个功能之前
完全一致。四个入口：

- `LoadClientAuth(caFile, denyFile)` 读 CA 池与吊销名单。名单文件读不到、或者有
  一行不合法都直接报错，不跳过：一份读不进来的吊销名单比没有名单更危险。
- `(*ClientAuth) TLSConfig(certFile, keyFile)` 组装 `tls.Config`。
- `(*ClientAuth) withClientCertAuth(next)` 是 HTTP 层闸门。
- `Fingerprint(cert)` 给运维脚本用，算出的格式与吊销名单一致。

两个关键取值：

`ClientAuth` 用 `tls.VerifyClientCertIfGiven`，不用 `RequireAndVerifyClientCert`。
标准库文档写的是两者都要求「发过来的证书必须有效」，区别只在没发证书时。取前者，
没带证书的请求能走到 HTTP 层拿到说明页；取后者握手直接失败，浏览器只画它自己的
错误页，我们连一句解释都插不进去。而「还没装证书」正是最常见的失败。

闸门同时检查 `PeerCertificates` 非空与 `VerifiedChains` 非空。后者为空说明这条链
没验过，宁可拒。`internal/server/auth_test.go` 里有一条用例专门钉这一点。

EKU：实测标准库会拦。`extendedKeyUsage` 只有 `serverAuth` 的客户端证书在握手
阶段就被拒，日志是 `x509: certificate specifies an incompatible key usage`。
闸门里仍然自己查了一遍 `hasClientAuthEKU`，是为了让这条性质不依赖标准库的实现
细节。没有 EKU 扩展的证书按 RFC 5280 视为可用于任何用途，所以只在「写了 EKU 但
不含 `clientAuth`」时拒绝。

吊销：标准库的 TLS 栈不做 CRL 与 OCSP 检查，所以用服务器侧的指纹名单。命中给
`403` 并记日志。名单在启动时读一次，改完要重启。

身份只进日志，不进请求上下文：页面级请求（不是 `/static/` 也不是 `/doc/*/res/`）
记一条 `subject` 与 `serial`，子资源不记，否则一个阅读页几十个请求会把日志淹掉。
日志里的 `subject` 与名单备注都过 `sanitizeMsg`，证书里的 CN 是外部输入。

两套 CSP：正文路由必须放宽（包内 HTML 普遍自带内联 `<script>`，标签页切换靠它），
主站严格。`nosniff` 与 `X-Frame-Options` 对正文同样有效，不需要放宽。
`frame-ancestors` 与 `object-src` 要单独写，因为它们不参与 `default-src` 回退，
漏写等于没防。

`setContentType` 对 `.html/.htm/.css/.js/.xml/.txt` 一律不带 charset。
原因是文档包的 HTML 内部声明 `charset=gb2312` 而字节是 GBK，Go 的
`mime.TypeByExtension(".html")` 返回 `text/html; charset=utf-8`，
而 HTTP 头的 charset 优先级高于 HTML 内的 `<meta>`，照搬会让浏览器按 UTF-8
解码 GBK 字节、全篇乱码。

缓存策略（本地工具的铁律是「改了立刻生效」远比省流量重要）：

| 资源 | 策略 |
|---|---|
| HTML 外壳（`/`、`/doc/{id}/`） | `no-store, must-revalidate` 加 `Pragma: no-cache` 加 `Expires: 0` |
| `/static/*` | `no-cache`，交给 `http.ServeFile` 走 `Last-Modified` / `ETag` 条件请求 |
| `/doc/{libId}/res/*`（正文资源） | `public, max-age=86400`（包内资源不随前端改动变化） |
| JSON 接口 | `no-store`（检索词会出现在 URL 与响应体里，被中间层缓存等于把一次查询留给下一个访客） |

错误文案不回显内部信息：资源不存在时不回显包内 zip 路径，目录解析失败时不回显
包在磁盘上的绝对路径，细节只进日志。`sanitizeMsg` 去掉控制字符，
因为错误文案里会带上用户可控的内容，换行能借此进入 JSON 响应与日志、
伪造出完整的一行假记录。

http.go 里每个超时都显式给出，不留 Go 的默认值（不限时）。慢速客户端靠这些超时被踢掉，
否则一个连接可以长期占住一个请求槽位。

ratelimit.go 是每 IP 一个令牌桶，定期扫掉闲置的桶，否则被大量不同源 IP 打的时候
这张 map 会一直涨。`clientIP` 默认只信 `RemoteAddr`，只有显式开了 `TrustProxy`
才认 `X-Forwarded-For`，否则任何人都能自己塞这个头，限流等于没有。

security.go 里的参数边界：`maxQueryRunes = 256`、`maxPageIndex = 10000`、
`maxSkipHits = 100000`。超长查询没有上限时会先撞上 `MaxHeaderBytes` 变成 `431`，
那个报错对调用方毫无信息量。

### internal/memguard — 内存自守

- 四档：软限清缓存，告警线记日志并降级，硬限记计数，回落到软限以下解除降级。
- `CurrentRSS()` 优先走平台实现（Linux `/proc/self/statm`，Windows 进程工作集），
  与 `docker stats`、任务管理器同一口径。其它平台退回 `runtime.MemStats` 的近似值
  （`Sys` 减 `HeapReleased`）。这个近似值偏差很大，实测容器里工作集 21 MB 时它能
  报到 146 MB，会让降级在错误的时点触发。
- `PublicStats()` 对外只给向上取整到 50 MB 倍数的 `memMB` 与三档 `level`。
  不给精确 RSS 的原因：匿名暴露实时内存等于给攻击者一把尺子，他能把负载精确压在
  软限附近，让守卫反复清标题索引、每次搜索都得重建。向上取整而不是向下，
  是因为冷启动 RSS 只有 19 MB，向下取整会显示成「内存 0 MB」。
- `level` 的阈值取自配置而不是写死，因为软硬限都能用命令行改，写死迟早对不上。
- `SetMemLimit` 包装 `debug.SetMemoryLimit`。必须显式设置，因为 Go 1.19+ 默认不感知
  cgroup 内存限制，会按宿主机总内存决定 GC 时机，一路涨到被容器 OOM Kill。
  `GOMEMLIMIT` 环境变量由 Go 运行时自己识别，`--mem-limit` 的默认值 300 MB 与
  `GOMEMLIMIT=300MiB` 是同一个数（314,572,800 字节）。

### cmd/web_ics — 装配与自检

main.go 的启动顺序：

1. 解析命令行。所有 flag 的默认值都填内置默认，解析完之后再由 `applySettings`
   按「命令行 > 环境变量 > 配置文件 > 内置默认」补值。
2. `--version` 短路退出。它不读配置也不生成模板，保持无副作用。
3. 加载配置文件、补值，把实际用的是哪个文件打进日志。
4. `--selftest` 短路退出。
5. `debug.SetGCPercent(50)`，更积极的 GC，代价是少量 CPU。
6. `memguard.SetMemLimit(--mem-limit)`。
7. `GOMAXPROCS` 上限压到 2。核数少的机器上不要用满所有核去跑 GC 与调度。
8. `doclib.Scan(--doc-root)`。
9. 建语料库 Store，`Refresh()` 做缺失检查。`--build-corpus` 时同步建完退出，
   否则把重建丢到后台 goroutine。
10. 建 `nav.Cache` 与 `server.Server`，再建内存守卫。守卫的 `onSoft` 回调要引用
    `Server` 的搜索索引，所以拆成「先 New 再 SetGuard」两步，避免循环依赖。
11. 起 HTTP 服务，等待 `SIGINT` 与 `SIGTERM`，5 秒优雅退出。

#### 配置文件（config.go）

配置文件解决的是「每次启动都要带一长串参数」，最常见的需求是把文档库目录固定下来。

格式是 `key = value` 的纯文本。不用 JSON：这里最常填的是 Windows 路径，
JSON 要求把每个反斜杠写成两个，抄一次错一次。也不用 TOML：那要引新依赖，
而本项目的依赖只有 `golang.org/x/text`（GBK 解码），是必要的那种。
自己写的解析器一百来行，边界都有测试钉着。

查找顺序是 `--config` 指定的路径、可执行文件旁边、用户配置目录。
第一处用 `os.Executable()` 而不是 `os.Getwd()`：双击启动时两者一样，
从别的工作目录启动时不一样，而「配置放在 exe 旁边」这个约定必须按 exe 算。
一个都没找到就生成一份带注释的模板。

两条不能动的约定：

- 生成的模板里所有键都必须是注释状态。生成配置文件这个动作本身不该改变
  程序行为，否则用户什么都没改就发现行为和以前不一样。有测试钉这一条。
- 配置里的相对路径按配置文件所在目录解析，不是按工作目录。这样把 exe 和
  配置一起挪走之后 `doc-root = ./docs` 仍然指向它们旁边。代价是同一个相对路径
  写在命令行上和写在配置里含义不同，README 里写明了。
  配套要求：配置路径本身必须先转绝对，否则 `--config rel.conf` 时
  `filepath.Dir` 只得到 `.`，相对解析静默退化成按工作目录。

键名写错、类型不对、引号没闭合都直接报错并指出行号，不静默忽略。
静默忽略的话，用户会以为「我配了」而实际没生效，这种问题很难查。

`applySettings` 用 `flag.Visit` 拿到命令行显式指定过哪些 flag，只补没指定的那些。
动作类参数（`--version` / `--selftest` / `--build-corpus`）不在配置表里：
它们表达「这次要做什么」，不是配置。


内存守卫的 `onSoft` 回调做两件事：`trees.Drop()` 清目录树缓存，`si.Release()`
释放标题索引。以后新增任何随文档规模增长的常驻内存，都必须加进这个回调。

selftest.go 在进程内用 `net/http/httptest` 起测试服务，逐项验证关键路径。
它会把 `IPRate` 设为 0（自检连打几十个请求，限流会误伤），把 `DebugHealth` 置真，
并只为最小的那个包建一份语料分片（全量建库约 4 分钟，不适合放进自检）。

客户端证书那几项不现造证书、也不起 TLS 监听：用 `NewClientAuth` 构造一个空 CA 池
的闸门，再手工构造带 `tls.ConnectionState` 的请求打 `Handler`。自检的价值在于
「什么都没装也能跑」，真实的握手、链校验与 EKU 拦截由 `internal/server` 的单元
测试覆盖。

### web/ — 前端

这个目录同时是一个 Go 包。`web.go` 用 `//go:embed` 把同目录的 HTML / CSS / JS
编译进二进制，所以 `dist/web_ics.exe` 单独拷到任何目录都能跑，不依赖外置的 `web/`。

运行时取文件走 `internal/server` 的 `serveWebAsset`，按优先级：

1. `WEB_ICS_WEB_ROOT` 指定的目录。设了就只看它，找不到就落到内嵌那份，不会再去读 `./web`。
2. 进程工作目录下的 `web/`。开发时的默认路径，改前端不用重编译。
3. 编译进二进制的那份。

前两步都要先 `stat` 到具体文件才用，所以目录配错时会自然回退，不会 404。
内嵌文件的修改时间是零值，给不出 `Last-Modified`，也就没有 304。
内嵌模式本来就要重编译才能换文件，条件请求没有意义，不额外补 ETag。

`Dockerfile` 不再 `COPY web /app/web`，运行阶段只有那一个可执行文件。

| 文件 | 职责 |
|---|---|
| `index.html` 与 `home.js` | 首页：领域导航、库表格（本地排序）、搜索面板、分页 |
| `viewer.html` 与 `viewer.js` | 阅读页：目录树懒加载、深链定位、正文 iframe、本文档内搜索、分栏拖动 |
| `mem.js` | 顶栏内存指标，首页与阅读页共用一份轮询（每 10 秒拉 `/healthz`） |
| `style.css` | 全部样式 |

几个必须保持的设计点：

- 搜索请求可取消并带序号。正文检索能扫好几秒，用户改词或翻页时旧请求仍在跑，
  其响应会覆盖新查询的结果。用 `AbortController` 加递增 `seq`，回调开头
  `if (mySeq !== seq) return;` 直接丢弃过期响应。
- `Failed to fetch` 必须翻译成人话，并区分「用户主动取消」。漏掉 `AbortError` 判断的话，
  正常操作（切标签、改词）会冒出假失败，比原来的报错更让人困惑。
- 深链展开必须逐层串行 `await`，因为下一层的节点在上一层加载完之前不存在。
  `expandNode(st, node)` 返回 Promise，`expandChain(path, i)` 递归 await。
  fire-and-forget 的 `toggle()` 展开到第二层就断。
- iframe 用 `sandbox="allow-scripts"`，不加 `allow-same-origin`。两者同时给等于没沙箱，
  同源 frame 可以自己把自己的 sandbox 属性摘掉。文档正文也用不到 same-origin：
  包内文件里 `localStorage`、`sessionStorage`、`document.cookie`、`document.domain`
  的命中数为 0。代价是「回到顶部」按钮取不到 `contentDocument`，
  那里本来就包了 try/catch，跨源时静默忽略。
- 正文里的 `src` 与 `href` 必须逐段编码（`resURL`），`#` 要单独处理，
  否则会被浏览器当锚点截断。
- 改了前端资源记得把 `web/*.html` 里的 `?v=N` 一起加一（当前 `v=21`），
  让已经中毒的缓存被当作新 URL 重新拉取。

---

## 数据流与关键流程

### 请求路由与中间件链

```
请求 -> withSecurityHeaders（安全头，含按路由选择的 CSP）
     -> withClientCertAuth（仅在配了 client-ca 时：没证书 401、已吊销 403）
     -> withMethodGuard（非 GET/HEAD 返回 405）
     -> withRateLimit（/api/search、/api/grep 按 IP 限流，超出返回 429）
     -> withGate（降级或信号量满返回 503）
     -> mux 分发
```

### 首页

```
GET /               -> index.html（no-store）
GET /api/categories -> Index.Categories()（规则表现场推导，含 Count）
GET /api/libs       -> Index.List() 精简成列表页需要的字段（含 CategoryOf）
```

前端拿到库列表后在浏览器本地做领域过滤与排序，翻页只用于搜索结果。

### 目录树懒加载与深链定位

```
GET /api/nav?lib=<libId>&parent=<nodeId>
  -> Cache.Get(libId, loader)   （命中直接返回，未命中则解析，解析期间不持锁）
       loader: OpenPackage -> nav.Parse（navi.xml -> .hhc -> FileList.xml）
  -> tree.ChildrenOf(parent)    （parent 为空返回顶层）
  -> {lib, parent, items:[...]}
```

深链，也就是从搜索结果点进文档：

```
GET /api/nav?lib=<libId>&node=<nodeId>   标题命中带 nodeId
GET /api/nav?lib=<libId>&url=<相对URL>    正文命中只有 URL，需反查
  -> tree.Path(node) 或 tree.PathByURL(url)
  -> {lib, node, path:[根...目标]}
  -> 解析不到时返回空数组（200），不是 404
```

前端拿到 `path` 后逐层串行展开，最后选中目标节点、更新面包屑。
解析不到时退化为打开第一篇，不留白屏。有些正文页根本没被 `navi.xml` 收录，
这是常态不是异常。

### 正文直出

```
GET /doc/{libId}/res/{包内路径}
  -> 只接受 res/ 前缀，映射成 zip 内 "resources/{包内路径}"
  -> OpenPackage -> Reader.Lookup（精确，再小写折叠）
  -> 大文件（4 MiB 以上）单独限流
  -> setContentType（文本类型不带 charset）
  -> Reader.Stream -> io.Copy -> 响应
```

`Reader.Lookup` 失败时只回「资源不存在」，包内路径只进日志。

### 融合检索

```
GET /api/search?q=…&scope=…&page=…&limit=…&firstLimit=…&cat=…&lib=…

  keep := 领域过滤
  if lib != "" { keep := 单库 且 领域过滤 }        （标题段与正文段共用）

  1. 标题段（精确、即时、完整）
       searchTitlesAll(q, keep)
         -> ensure() 保证索引可用（可能触发构建）
         -> 全量扫 titles 列，按 (title, url) 合并主题，组内按 (libName, url) 去重
         -> 排序：Score（精确 > 词首 > 词中），再标题长度，再标题，再 url
       res.TitleN = len(结果)                     唯一主题数，与分页解耦

  2. 算本页 offset（把两段看成一条连续结果流）
       第 1 页取前 FirstPageSize 条，第 k 页取接下来的 PageSize 条
       第 1 页让出约 1/4 给正文段（contentFirstPageShare = 4）

  3. 正文段
       语料库可用 -> corpusContent（扫全部已建好的包，total 精确）
       否则       -> fusedContent 加 Grep（逐包兜底）
       顺带给本页的标题命中补正文摘要（enrichTitleHits）

  4. 合并：标题段在前，正文段在后，填 PageInfo 与 ContentDig
```

### 语料库构建与新鲜度判定

```
启动: Store.Refresh()
        对 59 个包逐个 shardFresh()（59 次 stat 加读 8 字节头）
        就绪 = 分片存在 且 分片 mtime >= 包 mtime 且 magic 匹配
               且 formatVer 匹配 且 ExtractRevision 匹配
      -> 缺失的丢到后台 goroutine，BuildAll() 串行逐个 BuildShard()
      -> BuildShard: 打开包
                     -> buildBreadthMap（解析目录树拿章节路径）
                     -> 逐篇 ExtractText / ExtractTitle，流式写 textBlob
                     -> 写 offsets / metaOffsets / metaBlob / urlIdx
                     -> WriteAt 回填头部 -> Sync -> Rename（原子替换）
      -> 成功后立刻标记该包为可用（ReadyCount 增加）
```

`--build-corpus` 走同一条路径，只是同步执行完再退出。

### 标题索引的构建与释放

```
查询路径: ensure()
  cols 已挂载        -> 直接返回
  refreshRunning     -> sleep 5 ms 重试（等别人的结果，不自己再建一份）
  refreshDead        -> CAS 复位成 idle，然后 fallthrough 去抢构建权
  refreshDone/Idle   -> CAS 抢 refreshRunning
                        抢到   -> refreshIndex()（只持 refreshMu，不持 buildMu）
                        没抢到 -> 回到循环等结果

refreshIndex()
  持 refreshMu 构建（数秒）
  出锁后 CAS Running -> Done：
    成功 -> 挂载 cols
    失败（说明构建期间被 Release 打成 Dead）-> 丢弃这份结果，返回 false

Release()（内存守卫每 5 秒可能调用）
  持 buildMu
  若有在途构建 -> 最多等 buildGrace(10 s) 让它落地，超时才 CAS 成 Dead
  cols 非空 -> 清空
  cols 为空 -> 顺便把 Dead 复位成 idle，否则后续查询会一直看到 Dead 反复重建
```

不变量：任何时刻至多一个 goroutine 在构建；被 `Release` 打断的构建结果永不挂载；
查询路径永不持 `mu.RLock` 去拿 `buildMu`。

---

## 接口说明

所有接口只接受 `GET` 与 `HEAD`。错误统一返回 `{"error": "…"}`，
状态码为 `400` / `404` / `405` / `429` / `500` / `503`。

### GET / — 首页外壳

返回 `web/index.html`，`Cache-Control: no-store`。

### GET /doc/{libId}/ — 阅读页外壳

返回 `web/viewer.html`，`Cache-Control: no-store`。深链参数：

| 参数 | 说明 |
|---|---|
| `url` | 包内相对路径（相对 `resources/`）。必须整段编码，连斜杠一起，否则路径里的 `#` 会被当锚点截断 |
| `node` | 目录节点 ID。优先按它定位，失败再按 `url` |

### GET /doc/{libId}/res/{包内路径} — 正文资源直出

- 只接受 `res/` 前缀，映射到 zip 内的 `resources/{包内路径}`。
- 查找顺序是精确路径，再小写折叠回退（大小写不敏感）。
- 文本类型不带 charset。`Cache-Control: public, max-age=86400`。
- 超过 4 MiB 视为大文件，同时最多 2 个，超出返回 `503`。
- 不存在的资源返回 `404`，且不回显包内路径。

### GET /api/libs — 文档库列表

```json
{ "total": 59, "items": [ {
    "libId": "AZH0312X", "libName": "…", "productType": "…",
    "productVersion": "…", "libVersion": "…", "issueDate": "…",
    "topicNumber": 1234, "sizeMB": 210,
    "categoryKey": "datacom", "categoryName": "数据通信"
} ] }
```

### GET /api/categories — 产品领域分类

```json
{ "total": 7, "items": [ { "key": "datacom", "name": "数据通信", "order": 30, "count": 40 } ] }
```

只返回实际出现过的分类。当前 59 个包的分布是：无线 4、数据通信解决方案 8、
数据通信 40、网络管理 3、数据存储 1、计算 2、华为云 1。

### GET /api/nav — 目录树

| 参数 | 必填 | 说明 |
|---|---|---|
| `lib` | 是 | 库标识 |
| `parent` | 否 | 父节点 ID，为空返回顶层节点 |
| `node` | 否 | 深链，返回「根到该节点」的链 |
| `url` | 否 | 深链，按包内相对 URL 反查节点链 |

按层返回：

```json
{ "lib": "…", "parent": "", "items": [
    { "id": "…", "parent": "", "title": "…", "url": "…", "hasKid": true } ] }
```

深链返回：

```json
{ "lib": "…", "node": "…", "path": [ /* 根到目标 */ ] }
```

`path` 为空数组表示该节点或 URL 不在目录树里（正文命中常见），
前端据此退化为打开第一篇。`node` 与 `url` 同时给出时优先按 `node` 解析。

### GET /api/search — 融合检索（界面入口）

| 参数 | 默认 | 约束 | 说明 |
|---|---|---|---|
| `q` | 无 | 256 字符以内 | 检索词。为空时返回空结果，不报错 |
| `scope` | `all` | `all` / `title` / `content` | 见下方说明 |
| `page` | `1` | 1 到 10000 | 页码，从 1 开始 |
| `limit` | `20` | 200 以内 | 第 2 页起每页条数 |
| `firstLimit` | `100` | 500 以内 | 第 1 页条数 |
| `cat` | 空 | 领域键 | 领域过滤 |
| `lib` | 空 | 库标识 | 非空时限定在这一个文档包内检索 |

> `scope=content` 目前与 `all` 等价，因为代码里所有分支都只判断 `scope != "title"`。
> 保留该取值是为了将来能真正只搜正文。见 [PLAN.md 的待办事项](PLAN.md#待办事项)。

响应：

```json
{
  "query": "接口",
  "libScope": "",            // 空 = 搜全库；非空 = 限定在该文档包内
  "total": 277473,           // 全量命中数（精确）= titleN + content.matched
  "titleN": 6941,            // 唯一标题主题数（已合并重复收录，不受分页影响）
  "items": [ /* 本页结果，见下 */ ],
  "page": {
    "pageIndex": 1, "pageSize": 20, "firstPageSize": 100,
    "total": 277473, "hasPrev": false, "hasNext": true, "more": true
  },
  "content": {
    "enabled": true, "scanned": 828295, "scannedLib": 59, "totalLibs": 59,
    "matched": 270532, "moreLibs": false, "incomplete": false,
    "timeout": false, "elapsedMs": 1008
  },
  "corpus": { "enabled": true, "ready": 59, "total": 59, "building": false },
  "indexNode": 831354,
  "indexMs": 7966,
  "elapsedMs": 1051
}
```

`items[]` 的字段是标题命中与正文命中的并集，由 `source` 区分：

| 字段 | 类型 | 出现在 | 说明 |
|---|---|---|---|
| `source` | string | 全部 | `title`（标题命中）或 `content`（正文命中） |
| `libId` / `libName` | string | 全部 | 所属文档包。标题命中取组内包名最小的那个 |
| `title` | string | 全部 | 标题命中是标题，正文命中是正文页标题 |
| `url` | string | 全部 | 相对 `resources/` 的 URL |
| `nodeId` / `parentId` | string | 标题 | 目录节点 ID 与父节点 ID，深链定位用 |
| `breadth` | string | 全部 | 章节路径，形如 `命令参考 / 安全 / PKI配置命令` |
| `snippet` | string | 全部 | 摘要（含 `<em>` 高亮）。标题命中也带；正文里没有检索词时给文档开头一段 |
| `count` | int | 全部 | 该篇命中次数 |
| `score` | int | 全部 | 排序权重，`0` 是标题、`1` 是正文，越小越靠前 |
| `groups` | array | 标题 | 收录该主题的全部文档包，长度大于 1 时前端渲染成列表。元素含 `libId` / `libName` / `breadth` / `url` |

语义约定：`total` 是全量命中数。`content.incomplete` 表示扫描未覆盖全部范围
（还有包没建好语料），不是结果被丢弃。界面文案必须写成「已就绪 X/N」。

### GET /api/grep — 底层正文检索通道

界面不走这个接口（界面走 `/api/search`）。它保留给脚本与排障直接调用，
语义与融合检索的正文段一致。

| 参数 | 默认 | 约束 | 说明 |
|---|---|---|---|
| `q` | 无 | 2 到 256 字符 | 少于 2 个字符返回 `400` |
| `lib` | 空 | 库标识 | 限定单库 |
| `limit` | `50` | 200 以内 | 本页最多取回多少条，不是结果上限 |
| `skip` | `0` | 100000 以内 | 跳过前 N 条命中，用于确定性分页 |
| `cat` | 空 | 领域键 | 领域过滤，在命中结果里筛 |

```json
{
  "query": "接口", "total": 26, "matched": 26,
  "scanned": 323, "scannedLib": 1, "totalLibs": 1, "skip": 0,
  "more": false,
  "incomplete": false, "timeout": false,
  "elapsedMs": 47,
  "items": [ { "libId": "…", "libName": "…", "docId": "…", "url": "…",
               "title": "…", "breadth": "…", "snippet": "…", "count": 3 } ]
}
```

`incomplete` 等于 `truncated || timeout`，含义是扫描未覆盖全部范围，是进度信息。
已有命中一条都不会丢。

### GET /healthz — 健康检查

```json
{ "ok": true, "degraded": false, "memMB": 50, "memLevel": "normal" }
```

`memMB` 是向上取整到 50 MB 倍数的近似值，`memLevel` 是 `normal` / `high` / `critical`。
降级时 `ok:false` 但 HTTP 仍是 `200`，因为降级是正常的保护状态，不该让容器重启。

开了客户端证书认证之后这个接口也要证书，没有路径例外。探针与监控要带证书，
或者把探针跑在容器内部用同一份证书。

### GET /debug/healthz — 详细指标（默认关闭）

需要 `--debug-health` 或 `WEB_ICS_DEBUG_HEALTH=1`，否则返回 `404`。

```json
{
  "ok": true, "rssBytes": 201326592, "rssMB": 192, "degraded": false,
  "softLimitHits": 0, "hardLimitHits": 0,
  "cachedLib": "AZH0312X", "cachedNodes": 1234,
  "treeCacheHits": 42, "treeCacheMisses": 3,
  "inflight": 1, "maxConcurrency": 10, "libCount": 59,
  "corpusReady": 59, "corpusTotal": 59, "corpusBuilding": false, "corpusErr": ""
}
```

---

## 数据格式

### 包内 profile.xml

文档包有两种布局，都要认：

| 布局 | 位置 | 标签风格 | 用它的包 |
|---|---|---|---|
| A | 包根 `profile.xml` | 驼峰：`libId` / `libName` / `productType` / `productVersion` / `libVersion` / `issueDate` | `.hdx`，以及一部分 `.hwics` |
| B | `resources/infocenter_service/profile.xml` | 全小写、名字不同：`libid` / `resourcelibname` / `productname` / `productversion` / `resourcelibversion` / `issuedate` | HedEx 2.0 导出的 `.hwics` |

只读这几个字段：

```xml
<!-- 布局 A -->
<libId>…</libId> <libName>…</libName> <productType>…</productType>
<productVersion>…</productVersion> <libVersion>…</libVersion>
<issueDate>…</issueDate> <language>…</language> <topicNumber>…</topicNumber> <guid>…</guid>
```

两套都解析到同一个结构里，取值时驼峰优先、小写兜底。一个包里可能有多份
`profile.xml`（书签映射目录里也有同名的），所以先按「包根 > HedEx 位置 >
其它（路径短的优先）」排序，只取第一份能解析出内容的，免得把嵌套那份的字段混进来。

几个具体约定：

- 布局 B 的 `resourcelibname` 不采纳。它比文件名还差，例如
  `V800R021C00 产品文档_new` 丢了产品名还带个 `_new` 后缀。这类包用文件名当
  展示名更好，所以 `LibName` 只认布局 A 的 `libName`。
- 布局 B 的 `issuedate` 带时间（`2022-04-13 03:05:52`），而界面上那一列是「日期」，
  所以统一截到第一个空白之前。
- 布局 B 的 `resourcelibversion` 在实测的 4 个包里都是空的，那几个包的文档版本
  列因此显示为空（界面上鼠标停上去会说明这一点）。这是包本身没有这个信息，不是
  解析失败。已经翻过一遍确认，不用再找第二遍：包内所有非 HTML 条目
  （`GUID.xml`、`docnav.xml`、`meta_name_list.xml`、`meta_value_list.xml`、
  `pid_meta.xml`、`topic_meta.xml`、`MappingCMS.xml`、`FileList.xml`、`.hhp` 与
  `.hhk`）都没有文档版本；导航里那页「文档包信息 / 配套版本」列的是对应的产品版本
  （`V800R021C00SPC100` 这类）；文件名里也没有。对照另外 2 个正常的 `.hwics`，
  它们多一份包根的 `profile.xml`，`libVersion` 就在那里。

字段缺失时回退到文件名推断。包里一份 `profile.xml` 都没有时也走这条回退。

### 包内 resources/navi.xml（目录树首选来源）

```xml
<topics>
  <topic txt="标题" url="相对路径" id="节点ID">
    <topic txt="子标题" url="…" id="…"/>
  </topic>
</topics>
```

用 `xml.Decoder` 流式解析（`Strict = false`，宽容处理未转义字符），
用 ID 栈维护父子关系。`id` 缺失时依次用 `url`、`auto-<序号>` 兜底。

### 包内 .hhc（CHM sitemap，兜底来源）

结构见 `internal/nav/hhc.go` 的注释。两个必须记住的点：按 `</LI>` 弹栈而不是 `</UL>`，
以及文件声明 `gb2312` 但字节实际是 GBK、必须先转码。

### 语料分片 .icsc v2

格式定义见上文 [internal/corpus](#internalcorpus--磁盘语料库)。
`formatVer` 或 `ExtractRevision` 变更都会让旧分片自动失效并重建。

---

## 开发规范与工作流

### 代码风格

- 强制 `gofmt`。`build.sh` 第一步就是 `gofmt -l ./cmd ./internal`，
  有未格式化的文件直接退出。
- 强制 `go vet ./...`。
- 注释用中文，写为什么而不是做什么。函数名与变量名能表达做什么时不要再写一遍。
- 注释里出现的数字必须是实测或可推导的，不要写「大约」「可能」这类无法验证的说法。
- 新增的常量如果是为了对抗某个具体故障（上限、宽限、退避），注释里要写清
  漏掉它会怎样。本仓库大量注释都是这个形式，是刻意保持的风格。

### 命名与结构

- 一个包一个职责。`internal/htmltext` 被拆出来就是因为 `search` 与 `corpus` 要共用
  抽取逻辑，各写一份必然漂移。
- 跨包依赖用函数字段注入（`search.Options.Libs` / `OpenPackage` / `CategoryOf`），
  而不是让下层包直接 import 上层。这样融合检索可以脱离真实文档库跑通，
  也避免循环依赖。
- 平台相关代码用构建约束后缀（`rss_windows.go` / `rss_linux.go` / `rss_other.go`）。

### 提交与分支

- 主分支是 `master`。
- 提交信息用中文，第一行说明改了什么、为什么，例如
  `去掉 server.go 末尾多余空行，修 CI 的 gofmt 检查`。
- 不要在提交信息里写「优化」「完善」这类无法验证的词。

### 构建

```bash
go build ./...          # 快速编译校验
go test ./...           # 编译所有包（仓库当前没有 _test.go 文件）
bash ./build.sh         # 出交付产物，别手敲 go build
```

Windows 上要注意 `bash` 这个名字。系统自带的 WSL 启动桩
`%LOCALAPPDATA%\Microsoft\WindowsApps\bash` 是 `wsl.exe` 的符号链接，在 PATH 里
排在 Git 前面，而 Git for Windows 只把 `cmd` 目录加进 PATH、`bin` 没加，
所以 cmd 与 PowerShell 里的 `bash` 会去调 WSL，报「适用于 Linux 的 Windows
子系统没有已安装的分发」。要用就写 Git Bash 的全路径，见 README 的 Windows
本机运行一节。

`build.sh` 比手敲多做五件事：

1. 校验工具链不低于 1.25。低于此版本的标准库有 34 条已公开漏洞，
   其中 4 条在 HTTP 请求路径上。
2. 跑 `gofmt -l` 与 `go vet`，不过就停。
3. 加 `-trimpath`。不加的话二进制里会留下构建机上的绝对路径
   （源码目录与 `go/pkg/mod` 模块缓存目录），交付出去等于把构建环境告诉别人。
4. 两个平台一起出（Windows `web_ics.exe` 与 Linux `web_ics-linux-amd64`），
   纯静态链接。最后自己 grep 一遍确认二进制里没有构建机路径与模块缓存路径。
5. `.git` 存在但不可用时（被清空、或只剩一个空目录）自动加 `-buildvcs=false`
   继续构建。不加的话 `go build` 会报 `error obtaining VCS status: exit status 128`，
   那句话完全看不出跟 git 有关。`.git` 整个不存在时 `go build` 本来就会跳过，
   不用管。

### 持续集成

`.github/workflows/ci.yml` 在每次 push 与 PR 上跑四个 job：

| job | 内容 |
|---|---|
| `verify` | `go test ./...`（编译校验），`bash ./build.sh`，`./dist/web_ics-linux-amd64 --version` 产物冒烟，上传 artifact |
| `windows` | Windows 上编译加 `--version` 冒烟（第二交付平台单独验证） |
| `security` | `govulncheck ./...`，只报代码里真的调用到的漏洞，有可达漏洞直接失败 |
| `docker` | `docker build`，至少验证 Dockerfile 能构建出镜像 |

同一分支上新的推送会取消上一次仍在跑的构建（`concurrency.cancel-in-progress`）。

> 用 `bash ./build.sh` 而不是 `./build.sh`：仓库里它的模式是 `100644`，
> Windows 上开发的仓库拿不到可执行位，直接执行会 `Permission denied`。
>
> 行尾由 `.gitattributes` 统一钉成 LF。仓库在 Windows 上开发，而 CI 和 Docker 都在
> Linux 上跑。CRLF 的 `build.sh` 会因 shebang 带 `^M` 执行失败，
> CRLF 的 `.go` 文件会被 `gofmt -l` 判成未格式化。

---

## 测试与调试方法

### 一、内建自检（首选，不开浏览器）

```bash
web_ics --selftest --doc-root <文档库目录>
```

在进程内用 `net/http/httptest` 起测试服务，逐项断言。当前 60 项全 PASS，覆盖：

| 分组 | 覆盖内容 |
|---|---|
| 基础 | 扫描文档库、`/api/libs`、首页、阅读页品牌字、抽样库端到端（5 个库） |
| 目录 | 顶层非空、按 `node` 解析深链、按 `url` 反查深链、未知节点返回空链、大目录首次展开耗时 |
| 正文 | 正文直出（断言 `Content-Type` 不含 `charset=utf-8`）、大小写不敏感回退、不存在资源 404、路径穿越 404 |
| 安全 | 非 GET/HEAD 返回 405、安全头四项、正文路由 CSP 放宽且仍禁嵌套、超长检索词 400、错误响应不回显内部路径 |
| 访问控制 | 客户端证书闸门：不带证书被拒、链未校验时被拒、带有效证书放行、吊销名单命中被拒 |
| 缓存 | 静态资源 `no-cache`、首页外壳 `no-store`、接口响应 `no-store` |
| 检索 | 融合检索、标题命中总数精确、首屏标题与正文混排、标题排在正文前、热查询耗时、精确匹配优先排序、分页零重叠且顺序稳定、标题按主题合并、限定单库、单库检索正文只扫本文档、领域过滤 |
| 语料 | 建分片、只扫已建好的包、状态如实上报、命中不越界 |
| 中文 | 正文检索能解出中文（GBK 转码回归） |

发布二进制前必须跑一遍，这是回答「冻结产物里到底是新代码还是旧代码」最快的手段。

### 二、客户端证书认证（要真证书）

自检与单元测试都覆盖了认证逻辑：前者用空 CA 池加手工构造的连接状态打 `Handler`，
后者现造证书起真 TLS 服务。但那两条都在进程内。要验真实二进制加真实证书，按下面走。

先造一套证书。不需要扩展文件：扩展写在 CSR 上，签发时用 `-copy_extensions copy`
拷过去。这样每条命令都是单行，在 cmd 里也能直接贴。

```bash
mkdir -p /tmp/certs && cd /tmp/certs

openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.crt -days 3650 \
  -subj "/CN=test CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"

openssl req -new -newkey rsa:2048 -nodes -keyout server.key -out server.csr \
  -subj "/CN=127.0.0.1" \
  -addext "subjectAltName=IP:127.0.0.1" \
  -addext "extendedKeyUsage=serverAuth" \
  -addext "keyUsage=digitalSignature"
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 365 -copy_extensions copy

openssl req -new -newkey rsa:2048 -nodes -keyout alice.key -out alice.csr \
  -subj "/CN=alice" -addext "extendedKeyUsage=clientAuth" \
  -addext "keyUsage=digitalSignature"
openssl x509 -req -in alice.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out alice.crt -days 365 -copy_extensions copy

# 吊销名单：alice 的指纹
openssl x509 -in alice.crt -noout -fingerprint -sha256 |
  sed 's/.*=//; s/://g' | tr 'A-Z' 'a-z' | sed 's/^/sha256:/' > deny.txt
```

> 两张证书都要带 `keyUsage=digitalSignature`。客户端证书只写 `extendedKeyUsage`
> 时，Windows 的证书选择器可能不认，浏览器就不会把它递出去。表现是服务端收到一个
> 没有证书的握手，页面显示 401 的「需要客户端证书」，看不出是哪一侧的问题。

> 握手超时也是个坑。标准库的 `http.Server` 拿 `ReadHeaderTimeout` 当作 TLS 握手的
> 截止时间，默认给的是 10 秒。开了客户端证书认证之后，用户要在浏览器弹出的框里挑
> 证书，10 秒经常不够，表现同样是「点确定没反应」，服务端日志里是一句握手 EOF。
> 所以 `NewHTTPServer` 在 `tlsCfg` 非空时把 `ReadHeaderTimeout` 放宽到 60 秒。
> 实测：不完成握手的连接，改之前 11 秒被断，改之后 61 秒。

> Windows 上要装两处：`ca.crt` 进「受信任的根证书颁发机构」（本地计算机或当前
> 用户都行），客户端证书（`alice.pfx`）进「个人」。装完可以用
> `certutil -user -store My` 看个人库、`certutil -store Root` 看本地计算机的
> 受信任根，确认装没装上、有没有私钥。

> 在 Git Bash 里跑要小心路径转换。`-subj "/CN=alice"` 里的 `/CN` 会被 MSYS 当成
> 路径，改写成 `E:/Develop/Git/CN=alice`，openssl 报
> `subject name is expected to be in the format /type0=value0/...`。三个办法：
>
> - 开头加一句 `export MSYS_NO_PATHCONV=1`（推荐）；
> - 把 `-subj "/CN=alice"` 写成 `-subj "//CN=alice"`，双斜杠会被还原成单斜杠；
> - 或者改用 cmd，cmd 不做这种转换。

> `-copy_extensions copy` 会把 CSR 里的全部扩展拷进证书。自己的 CSR 没问题，
> 但不要拿它去签别人给的 CSR，那样对方能在 CSR 里塞 `CA:TRUE` 之类的扩展。

> 在 cmd 里跑的话路径要写成 `E:\...`：给 `web_ics.exe` 传 `/tmp/certs/ca.crt`
> 会报 `The system cannot find the path specified`。

起服务，前台跑方便看日志：

```bash
./dist/web_ics.exe --doc-root <文档库目录> --addr 127.0.0.1:8443 \
  --tls-cert /tmp/certs/server.crt --tls-key /tmp/certs/server.key \
  --client-ca /tmp/certs/ca.crt --client-cert-deny /tmp/certs/deny.txt
```

另开一个终端发请求。不要用 curl：Windows 上的 curl 是 Schannel 后端，不能从
PEM 文件导入客户端证书（报 `schannel: Failed to import cert file`），也不认
`--cacert`。用 `openssl s_client`：

```bash
# 不带证书，期望 401
printf 'GET / HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n' |
  openssl s_client -connect 127.0.0.1:8443 -CAfile /tmp/certs/ca.crt \
    -verify_return_error -quiet 2>&1 | grep -m1 -o 'HTTP/1\.[01] [0-9]*'

# 带 alice 的证书，期望 200
printf 'GET / HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n' |
  openssl s_client -connect 127.0.0.1:8443 -CAfile /tmp/certs/ca.crt \
    -verify_return_error -quiet -cert /tmp/certs/alice.crt -key /tmp/certs/alice.key \
    2>&1 | grep -m1 -o 'HTTP/1\.[01] [0-9]*'
```

`grep -m1` 不能换成 `head -1`：OpenSSL 3.5 会先往 stderr 打一行进度信息，
合并输出之后第一行不是状态行。

把 alice 的指纹填进 `deny.txt` 再重启，同一张证书就变成 403。换一张
`extendedKeyUsage` 只有 `serverAuth` 的证书，握手阶段就断，服务端日志里是
`x509: certificate specifies an incompatible key usage`。

浏览器里试：CA 装进系统信任库，客户端证书导成 PKCS#12 再双击导入
（`openssl pkcs12 -export -out alice.pfx -inkey alice.key -in alice.crt`）。
Firefox 用自己的证书库，要单独导一次。

### 三、命令行核验清单

```bash
BASE=http://127.0.0.1:8080

curl -s --noproxy '*' $BASE/healthz                 # ok:true degraded:false
curl -s --noproxy '*' $BASE/api/categories          # 7 个领域带数量
curl -s --noproxy '*' $BASE/api/libs | head -c 300   # 59 条，含 categoryKey

# 目录树首层
curl -s --noproxy '*' "$BASE/api/nav?lib=AZH0627H" | head -c 300
# 深链（两种形态）
curl -s --noproxy '*' "$BASE/api/nav?lib=AZH0627H&node=index"
curl -s --noproxy '*' "$BASE/api/nav?lib=AZH0627H&url=dg_ac_r8c10%2Findex.html"

# 正文直出：关键看 Content-Type 是 text/html 而不带 charset
curl -s --noproxy '*' -D - -o /dev/null "$BASE/doc/AZH0627H/res/dc/ar_preface.html"

# 大小写回退：用小写请求驼峰文件，期望 200
curl -s --noproxy '*' -o /dev/null -w '%{http_code}\n' \
  "$BASE/doc/AZI0130X/res/public_sys-resources/tabSection.js"

# 仍应 404 的两类
curl -s --noproxy '*' -o /dev/null -w '%{http_code}\n' "$BASE/doc/AZH0627H/res/no-such-file.html"
curl -s --noproxy '*' -o /dev/null -w '%{http_code}\n' "$BASE/doc/AZH0627H/res/../../../../windows/win.ini"

# 融合检索（首次含建索引）
curl -s --noproxy '*' "$BASE/api/search?q=%E5%8D%8F%E8%AE%AE%E6%A0%88&limit=3" | head -c 600
# 单库加分页
curl -s --noproxy '*' "$BASE/api/search?scope=title&q=TCP&cat=datacom&limit=5&page=2"

# 底层正文通道
curl -s --noproxy '*' "$BASE/api/grep?q=TCP%E9%87%8D%E4%BC%A0&limit=10" | head -c 600

# 缓存策略
curl -s --noproxy '*' -D - -o /dev/null $BASE/ | grep -i cache-control
curl -s --noproxy '*' -D - -o /dev/null "$BASE/static/style.css" | grep -i cache-control
```

`lib=` 后面的 ID 从 `/api/libs` 的 `libId` 字段取。

### 四、界面验证（需要真实渲染）

`agent-browser` 不支持 Windows。可行的替代是直接调用 Chromium 二进制做 headless 截图：

```bash
CHROME="C:/Users/<用户>/.agent-browser/browsers/chrome-<ver>/chrome.exe"
"$CHROME" --headless --disable-gpu --no-proxy-server --no-sandbox \
  --hide-scrollbars --window-size=1600,1000 \
  --screenshot="<项目目录>\dist\home.png" \
  --virtual-time-budget=12000 "http://127.0.0.1:8080/"
```

- `--virtual-time-budget=12000` 给 JS 留出跑完的时间，异步加载目录树需要。
- `--no-proxy-server` 必须加，否则本机代理会劫持 localhost。
- `--screenshot=` 必须传绝对 Windows 路径。传相对路径会报
  `系统找不到指定的路径。 (0x3)`，看起来像权限问题，实际是 Chrome 不认这个相对路径。
- 编码问题尤其要看截图。中文乱码在 API 层面（HTTP 200 加字节数正常）完全看不出来。

需要验证交互（点击真的有用）时上 CDP：启动时加
`--headless --remote-debugging-port=9222 --remote-allow-origins=*`。
`--remote-allow-origins=*` 必须加，否则 WebSocket 握手直接 403。

- 用 `Input.dispatchMouseEvent` 发真实鼠标事件才是强证据。`Runtime.evaluate` 手调 JS
  等于自己调函数，绕过了事件绑定，即使事件绑定是坏的也会「通过」。
- 输入中文必须用 `Input.insertText`。`Input.dispatchKeyEvent` 只能发 ASCII 可打印字符，
  发中文是静默失败（输入框里什么都没有，看起来像搜索坏了）。而且 `insertText`
  不触发 `input` 事件，前端挂的是 `input` 防抖（200 ms），必须手工补一个
  `new Event('input',{bubbles:true})`。
- 整页导航会换掉 document，`window.fetch` 钩子随之消失。每次 `Page.navigate`
  之后必须重新挂钩子。`window.__reqs` 这种变量在跨页后是 `undefined`，别当空数组用。
- 断言「范围」不能只看请求 URL。钩子要同时 clone 响应体，确认全库检索的结果
  确实跨多个包，防止参数被静默忽略、退化成永远单库。

### 五、排障顺序（先确认环境，再怀疑代码）

`Failed to fetch` 报得很早（个位数毫秒）时，请求根本没发出去：服务没起、端口变了、
页面过期。按这个顺序走，别一上来改代码：

1. 用 `curl` 直接打那个接口。返回 200 就说明服务端好着。
2. headless 跑一遍真实交互。结果正常且无控制台错误，说明前端代码也好着。
3. 抓 Network 域看 `loadingFailed` 的 `errorText`。
4. 查服务日志有没有 panic。

判据：真实网络往返至少几十 ms，3 ms 就失败，问题在环境。

其它高频环境坑：

- 本机代理劫持 localhost。用 `curl --noproxy '*'`，
  并 `export NO_PROXY=127.0.0.1,localhost`。
- 后台启动的服务随 shell 任务结束被杀，要用工具的后台任务机制，不要用 `&`。
  典型误诊症状：`curl` 刚返回 200，隔几十秒浏览器连就 `ERR_CONNECTION_REFUSED`。
  判据是 `netstat -ano | grep :8080` 无监听且查不到进程，这时不要再怀疑代码。
- Git Bash 下往 `/tmp/` 写产物可能静默失败（返回 0 但文件不存在），
  一律写到项目内路径。
- Windows 上删不掉正在运行的 exe（`~` 备份残留）时，用 Restart Manager
  （`rstrtmgr.dll` 的 `RmStartSession` / `RmRegisterResources` / `RmGetList`）
  按文件路径反查持有者。别扫进程模块，那看不到数据文件句柄，
  会得出「没有进程占用」的错误结论。

### 六、内存压测

混合负载（库列表、顶层目录、二级目录、正文、图片）10 并发 60 轮，
每轮后读 `/debug/healthz` 的 `rssBytes` 看增长曲线。
判定标准是 `softLimitHits` 与 `hardLimitHits` 都保持 0。

内存数字要用对照实验归因，别猜。例：换到语料库后看到「首次搜索峰值 299 MB」，
第一反应是语料库太吃内存，用 `--corpus-dir <空目录>` 跑一次对照得到同值 299 MB，
说明峰值来自标题索引构建（既有行为），与语料库无关。

---

## 部署与版本发布流程

### 产物

| 产物 | 说明 |
|---|---|
| `dist/web_ics.exe` | Windows amd64，静态链接。自包含，拷到任何目录双击可用 |
| `dist/web_ics-linux-amd64` | Linux amd64，静态链接 |
| `ghcr.io/lovefirefly-26710/web_ics` | 容器镜像，Alpine，非 root，只读根文件系统 |

### 发布步骤

1. 确认工作树干净，`git status` 无未提交改动。
2. 跑自检：`web_ics --selftest --doc-root <文档库目录>`，要求全 PASS。
3. 跑构建：`bash ./build.sh`。它会校验工具链、格式、`vet`、交叉构建、构建机路径。
   版本号取自最近的 git tag，也可以用 `VERSION=x.y.z bash ./build.sh` 覆盖。
4. 验证产物：`./dist/web_ics.exe --version` 应打印刚注入的版本号。
5. Docker 路径（可选，本机无 Docker 时可在 CI 上验）：`docker compose build`，
   然后按 [README 的 Docker 步骤](README.md#docker--compose服务器推荐) 起一次，
   确认日志三行与 `/healthz`。
7. 打 tag 并推送：

   ```bash
   git tag v0.2.0
   git push origin v0.2.0
   ```

   `release.yml` 会构建两样产物并创建 Release，`ci.yml` 的四个 job 也会跑一遍。

### 版本号

版本号是 `cmd/web_ics/main.go` 里的包级变量 `version`。默认值 `0.1.0`
只供本地开发，发布时由 `build.sh` 与 `release.yml` 通过
`-ldflags "-X main.version=<tag>"` 覆盖，不要手工改默认值。

```bash
VERSION=0.2.0 bash ./build.sh                # 本地指定
git tag v0.2.0 && git push origin v0.2.0     # 发布，CI 从 tag 取
```

`--version` 的实际输出：

```
web_ics 0.1.0 (windows/amd64, go1.25.13)
```

格式串里不要写 `go` 前缀，`runtime.Version()` 自己已经带了。

注意：`build.sh` 里取版本号不能写成 `VERSION="${VERSION:-$(git describe ...)}"`。
仓库一个 tag 都没有时 `git describe` 返回 128，配合 `set -e` 与 `pipefail`
会把整个脚本杀掉（症状是 build.sh 只打印一行工具链就退出、产物根本没重建）。
必须写成 `VERSION=$(...) || true` 再用 `[ -n "$VERSION" ] || VERSION=...` 兜底。

### 前端资源的版本号

改了 `web/static/*` 或 `web/*.html` 之后，把 `web/index.html` 与 `web/viewer.html`
里的 `?v=N` 一起加一（当前 `v=21`）。这两处要同步改，漏一个就会出现
「一个页面拿到新版、另一个还是旧版」。

### Docker 构建的注意事项

- Dockerfile 里写死的 Go 版本、`go.mod` 的 `go` 指令、`build.sh` 的要求三者必须人工对齐。
  不一致时本地 `go build` 正常而 CI 与 `docker build` 失败。
- `go.mod` 的 `go` 指令会被 Go 1.21+ 自动抬高（当某个依赖要求更高版本时），
  所以升级依赖后要回看这一行。
- 运行阶段不需要 `COPY web`。前端已经编译进二进制，镜像里只有那一个可执行文件。
  要临时换界面就挂一个目录并设 `WEB_ICS_WEB_ROOT` 覆盖。
- 语料目录要在镜像里先建好并 `chown` 给非 root 用户。具名卷会继承镜像里该目录的属主，
  不先建的话挂上来的卷归 root，应用写不进去。

### Windows 没有安装包

只发免安装单文件 `dist/web_ics.exe`，不做安装包，也不注册卸载项。
原因见 PLAN 的 ADR-15，曾经踩到的坑记在 PLAN 的偏离记录里。

### 自动部署（deploy.yml）

push 到 main 后：构建镜像推到 GHCR，再 SSH 到服务器执行
`docker compose pull` 与 `docker compose up -d`。

它有一个总开关：仓库变量 `DEPLOY_ENABLED`。两个 job 都带
`if: vars.DEPLOY_ENABLED == 'true'`，没设时整个 workflow 显示为「跳过」（灰色）。

`build-image` 必须跟 `deploy` 一起关掉。它存在的唯一目的是让服务器能
`docker compose pull`；只关 `deploy` 的话，它每次 push 照样白构建一个镜像推到
GHCR，白费 runner，而且实测这一步在推送时会失败，push 仍然显示红色。

用 `vars` 而不是 `secrets` 做条件，是因为 `secrets` 在 job 级 `if` 里不可靠。
注意这是「跳过」而不是「静默失败」：总开关打开之后，secrets 缺哪一项
仍然会在「检查部署凭据」那一步明确报错退出。

不在服务器上编译，是因为目标机器是 2 核 1G，跑 `go build` 会把它压垮，
部署也会从几十秒拖到好几分钟。镜像在 CI 上构建，服务器只负责拉取。

几个容易出问题的地方：

- 镜像名必须全小写。GHCR 的路径不接受大写，而 `github.repository` 带原始大小写
  （`loveFirefly-26710/Web_ICS`），直接拼会 400。workflow 里用 `${GITHUB_REPOSITORY,,}` 转。
- `docker-compose.yml` 的 `image` 必须写全路径。写成 `web_ics:latest` 的话，
  服务器上的 `docker compose pull` 只会在本地找，永远拉不到新镜像。
- `concurrency` 不能设 `cancel-in-progress`。部署被打断可能停在
  「镜像已换、容器没起」的中间态。
- GHCR 的包默认私有。要么把包改成 public，要么在服务器上先 `docker login ghcr.io`。

---

## 关键陷阱与不变量

这一节是给改代码的人的速查表。每一条都在真实故障里出现过。

### 必须保持的不变量

1. 标题段与正文段用同一个 `keep` 判断。四处过滤点（标题段、正文段的库清单、
   正文命中输出、进度提示）漏掉任何一处都会串档。
   自检项「单库检索时正文只扫本文档」钉住这一点。
2. `ExtractRevision` 与 `formatVer` 变更必须配套。任何影响抽取输出的改动都要把
   `ExtractRevision` 加一，否则 `--build-corpus` 会空转（打印「59/59 已就绪」、
   1 秒退出），而结果是「改了代码但一点没变」。
3. 分片按包索引，不按 libID。本机有 4 组同名同 libID 的不同发行版。
4. `Lookup` 必须保留小写折叠回退。这不是容错，是复现文档自身引用行为的必要条件。
5. 文本类型的 `Content-Type` 不带 charset。
6. HTML 外壳不缓存，`/static/*` 协商缓存。
7. `Release` 后必须能重建，且 `refreshDead` 分支必须有 `fallthrough`。
8. `nextScan` 必须夹到 0（`corpus/scan.go`）。
9. 任何新增的常驻内存都要加进内存守卫的 `onSoft` 回调。

### 曾经踩过的坑

| 坑 | 症状 | 根因 |
|---|---|---|
| `sync.Once` 加可释放索引 | `fatal error: sync: unlock of unlocked mutex`，进程 abort | `Release` 复位 `Once` 与查询路径的 `Once.Do` 撞车。改用两把锁加原子状态机 |
| `refreshDead` 分支漏 `fallthrough` | 进程挂起（不是崩溃），goroutine 一直活着但没做事 | CAS 失败后 `continue`，导致所有人在 CAS 之间空转 |
| `Release` 不给宽限 | 索引永远建不起来，界面一直「检索中…」 | 2 ms 的释放节奏下 6 秒只完成 2 次构建且全被作废 |
| `ContentMaxDoc` 默认 0 | 正文命中数算得完全正确，但本页一条正文都看不到 | `snippets(text, q, 0)` 的循环一次都不执行，条目被 `continue` 丢掉 |
| `.hhc` 按 `</UL>` 弹栈 | 解析出 5 万节点但顶层只有 1 个，树塌成一条链 | 叶节点没有 `<UL>`，`<LI>` 有 55,664 个而 `<UL>` 只有 6,724 个 |
| `.hhc` 反复 `ToLower` | 10 MiB 的 `.hhc` 解析跑到 10 分钟超时 | O(n²)。改成一次性小写副本加游标，降到 382 ms |
| GBK 未前置解码 | 搜「接口」扫描 1433 篇、命中 0 | 正文是 GBK 字节、检索词是 UTF-8，字节序列完全不同 |
| `extractTitle` 漏了 GBK 转码 | 搜索结果标题一直是空的 | 与上一条同源 |
| `strings.ToLower` 用于切原串 | `slice bounds out of range` panic | 非法 UTF-8 字节被替换成 3 字节的 U+FFFD，长度变了 |
| 分页「重叠」 | 第 1 页尾部与第 2 页头部出现同一主题 | 不是分页 bug：同一篇文档被多个包收录，标题行本身有重复。按 `(title, url)` 合并后消失 |
| 分片文件名用 libID | 59 个包只落成 55 个分片，且缺失检查认为都就绪 | 4 组同名同 libID 的包互相覆盖 |
| 静态资源发 `max-age` | 首页只显示表头，一行数据都没有 | 旧 CSS 与新 JS 混用：DOM 里有 59 行，但没有网格布局规则 |
| 搜索结果直链裸文档页 | 用户报「搜不到全部文档」「文档页搜不了」「没有回首页入口」三个 bug | 其实是同一个：整页导航到包内 HTML，那份 HTML 里没有你的顶栏 |
| `dispatchKeyEvent` 发中文 | 输入框里什么都没有，搜索结果 0 条 | 它只能发 ASCII。必须用 `Input.insertText`，并补 `input` 事件 |
| `window.fetch` 钩子跨页失效 | 「请求不带 lib」的假失败 | 整页导航换掉了 document |
| 生产崩溃 `unlock of unlocked mutex` | Go runtime 直接 abort，`recover` 抓不住 | 锁倒置，见上 |

### 写代码时的自检问题

- 这个新字段的默认值在旧路径里是谁兜的？我这条路径兜了吗？
- 这个新状态能不能被内存守卫释放？释放后还能重建吗？
- 这个上限漏掉会怎样？注释里写清楚了吗？
- 我改的这个判据，是不是需要同步改另一处（版本号、自检断言、文档数字）？

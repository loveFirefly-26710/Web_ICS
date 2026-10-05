# Web_ICS — 轻量级华为 HDX/HWICS 产品文档阅读器

[![CI](https://github.com/loveFirefly-26710/Web_ICS/actions/workflows/ci.yml/badge.svg)](https://github.com/loveFirefly-26710/Web_ICS/actions/workflows/ci.yml)

在 2 核 / 1 GB 内存的服务器上阅读华为产品文档（`.hdx` / `.hwics`）。
Go 单二进制，零运行时依赖，Windows 与 Linux 共用同一份代码。

实测：59 个文档包 / 11.5 GiB，稳态内存约 190 MB，冷启动 0.6 s。
官方 ICS Lite 在同一负载下占用 756 MB。

---

## 关于这三份文档

| 文档 | 读者 | 写什么 |
|---|---|---|
| README.md（本文） | 使用者、部署者、运维 | 项目定位、核心功能、使用方式、整体结构、运维与排障 |
| [DEVELOPMENT.md](DEVELOPMENT.md) | 开发者、维护者、代码评审 | 开发环境、架构设计、编码规范、构建与测试流程、协作约定 |
| [PLAN.md](docs/PLAN.md) | 架构与技术负责人、后来接手的人 | 立项时的背景、目标、调研、预期范围与初步规划 |

改代码时请对照上表把变更同步到该更新的那份。三份文档互相矛盾是最容易发生、
也最难发现的一类问题。

术语以 [DEVELOPMENT.md 的术语表](DEVELOPMENT.md#术语表) 为准。
本文件的[实测数据](#实测数据)是全项目性能与资源数字的唯一出处，另外两份只引用不重写。

---

## 项目定位

华为产品文档包 `.hdx` / `.hwics` 本质上是标准 ZIP，内部是普通静态 HTML 加图片，
正文的资源引用全是相对路径。官方阅读器 ICS Lite 用 Java + Spring + 内嵌 Tomcat +
Derby + Aspose 共 190 个 jar / 266 MB 实现，自带 JRE，光常驻 JVM 就吃掉 1 GB。

在一台 2 核 1 GB 的机器上，原版没有可比性。而它真正有价值的部分只是
「管理 + 检索 + 阅读界面」这三件事，不是解密（包根本没加密），
也不是 Office 预览（这批文档里 Office 附件几乎为 0）。

所以这个项目把那三件事用 Go 重写，只保留真正需要的部分：

- 不解密、不解包。标准库 `archive/zip` 直读，不落盘解压，不占额外磁盘。
- 不改写 HTML。按包内原始层级直出，浏览器自己渲染就对了。
- 不建全量正文索引。全库 HTML 未压缩总量 6.48 GiB，1 GB 预算装不下。
  改为离线抽一份磁盘语料库（纯文本 1.61 GiB，落盘 1.73 GiB），每次查询顺序扫一遍
  （`strings.Index` 有 SIMD，实测 1.7~6 GB/s），一次查询就覆盖全部文档包。

一句话：它只做「能读、能搜、能跑在 1 GB 里」三件事，其余一律不做。

### 与官方的差距

| 维度 | 官方 ICS Lite | 本实现 |
|---|---|---|
| 技术栈 | Java 加 Spring 加内嵌 Tomcat 加 Derby 加 Aspose，190 个 jar / 266 MB | Go 单二进制，一个第三方依赖 |
| 同负载内存 | 756 MB（141 加 616 两个进程） | 约 190 MB（稳态） |
| 检索 | Lucene 8.11.2 全库正文倒排索引，索引规模 1~3 GB | 标题常驻列式索引加正文磁盘语料库全扫 |
| 交付 | 自带 JRE 的目录树 | 单二进制加 Docker 镜像 |
| 接口数 | 232 个 | 10 条路由 |

## 核心功能

### 浏览

- 首页左侧是产品领域导航（无线 / 数据通信 / 计算 等，服务端按规则表判定，
  前端不硬编码分类），右侧是文档库表格（文档包名称 / 产品版本 / 文档版本 / 日期，
  表头可排序）。
- 阅读页左侧是目录树（懒加载、可拖动分栏、目录内筛选、全部折叠），右侧是正文 iframe，
  顶栏有回首页、面包屑、本文档内搜索、上/下篇、回到顶部、内存指标。
- 目录树按层下发，不一次性推几万个节点。

### 检索

- 一个输入框，一次查询，同时命中标题与正文，标题命中靠权重排在前。这对应原版的
  Lucene `BooleanQuery` 加 `Occur.SHOULD`，标题权重 10、正文权重 1。
  没有「搜标题还是搜正文」的下拉框，范围由页面上下文决定。
- 范围：首页搜全库（59 个包），阅读页搜当前文档包，同一个接口加一个 `lib=` 参数。
- 结果卡片是「标题 / 正文摘要（`<em>` 高亮）/ 包名加章节路径」，无需点进去就知道讲什么。
- 标题命中按主题合并。同一篇文档被多个包收录时只出一条卡片，卡片内列出全部收录它的包。
- 精确分页。`total` 是全量命中数，不是「取回了多少条」；跨页零重叠，
  同一查询多次请求逐条一致。

### 工程

- 正文、图片、PDF 全程流式直出（`io.Copy` 加 32 KB 缓冲），内存占用与文件大小无关。
- 大小写不敏感的资源查找。文档里 URL 的大小写与包内文件名大面积不一致
  （目录 URL 26.4%、正文内部引用 35.5% 只能靠忽略大小写命中），
  不处理会有大面积图片 404 和正文标签页「点不动」。
- 内置 `--selftest`，不开浏览器、不开常驻服务就能验证二进制可用，当前 60 项全通过。
- 可选的客户端证书认证（mTLS）。配一个客户端 CA 就只放行持有证书的客户端，
  不需要用户名密码，也不需要额外的反向代理，见[开启客户端证书认证](#开启客户端证书认证)。
- 内存自守。四档阈值加容器硬限兜底，压力下先释放缓存再降级，不让容器 OOM Kill。

---

## 使用方式

### 环境依赖

| 项 | 要求 | 说明 |
|---|---|---|
| 运行 | Windows 10+ / Linux（x86-64） | 单个静态链接二进制，无 libc 依赖，无需任何运行时 |
| 构建 | Go 1.25 或更高 | 低于 1.25 的标准库有 34 条已公开漏洞，其中 4 条在 HTTP 请求路径上，`build.sh` 会直接拦下来 |
| 依赖 | `golang.org/x/text v0.21.0` | 唯一第三方依赖，用于 GBK 解码 |
| 磁盘 | 文档库 11.5 GiB，语料库 1.73 GiB，二进制约 7.5 MiB | 语料库可用 `--build-corpus` 随时重建，不必长期保留 |
| 内存 | 建议 512 MiB 以上可用 | 实测稳态约 190 MB，硬限 500 MB |
| Docker（可选） | Docker 20.10+ / Compose v2 | 主部署形态，构建阶段需要外网拉 `golang:1.25-alpine` |

> 文档包必须是 `--doc-root` 的直接子文件。扫描只读一层不递归，
> 套一层子目录会启动正常但库列表为空。

### 安装

#### Windows 本机运行

```powershell
# 1. 构建（需要 Go 1.25+）
go build -o dist/web_ics.exe ./cmd/web_ics

# 2. 起服务。必须从项目根目录启动
.\dist\web_ics.exe --doc-root "<文档库目录>" --corpus-dir docs/.web_ics-corpus --port 8080
```

`build.sh` 会多做几件事（工具链校验、`gofmt`、`vet`、双平台交叉构建、构建机路径核对），
要跑完整检查就用它。它是 bash 脚本，Windows 上别直接敲 `bash`，那会命中 WSL 的
启动桩，要用 Git Bash 的全路径：

```powershell
& "$env:ProgramFiles\Git\bin\bash.exe" ./build.sh
```

浏览器打开 <http://127.0.0.1:8080>。

> 为什么建议从项目根目录启动：`--corpus-dir` 这类相对路径按进程工作目录解析。
> 换到别的工作目录启动不会报错，而是把 59 个包重建成一份新的语料（约 4 分钟）。
> 前端资源不受影响，它已经编译在二进制里了。

#### Windows 免安装单文件

不想自己构建就从 [Releases](https://github.com/loveFirefly-26710/Web_ICS/releases)
下 `web_ics.exe`。它自包含，前端资源编译在里面，拷到任何目录双击就能用，
不需要旁边的 `web\` 目录，也不需要装任何东西。

```
Web_ICS\              （随便找个目录，把 exe 拷进去）
    web_ics.exe       第一次运行会自己生成 web_ics.conf
    web_ics.conf      文档库目录写在这里
    docs\             或者把文档包直接放这里（.hdx / .hwics，只扫这一层）
```

第一次双击运行会在 exe 旁边生成 `web_ics.conf`，里面的键全是注释状态，
所以生成这个文件本身不改变任何行为。把文档库目录填进去就行：

```
doc-root = ./docs
```

上面这个是最省事的写法：把文档包放进 exe 旁边的 `docs\` 就行。文档包在别处
（比如已经在某个盘的数据目录里）就把它填成那个目录的完整路径。

改完重启程序生效，详见[配置文件](#配置文件)。不想用配置文件也可以建快捷方式，
目标写 `"...\web_ics.exe" --doc-root "<文档库目录>"`。

双击后 Windows 上会自动打开浏览器（`--open` 在 Windows 默认开）。
文档库目录不存在时程序不会退出，会以空文档库启动并在控制台打出原因，
免得双击时控制台一闪而过什么也看不到。

首次启动会在后台建正文语料（59 个包约 4 分钟），这期间正文搜不到东西，标题能搜。
语料默认建在 `<doc-root>\.web_ics-corpus`，文档库只读时用 `corpus-dir` 指到别处。

#### Docker / Compose（服务器推荐）

前置：把 59 个 `.hdx` / `.hwics` 放到服务器上一个目录里（约 11.5 GiB）。
扫描只读一层、不递归，所以这 59 个文件必须直接躺在该目录下，不能放进子目录：

```bash
ls /你的文档库目录/*.hdx /你的文档库目录/*.hwics 2>/dev/null | wc -l   # 期望 59
```

```bash
# 1. 告诉 compose 文档库挂在哪。不用改 docker-compose.yml，写个 .env 就行：
#      echo 'DOC_DIR=/你的/文档库目录' > .env
#    端口默认是 8080:8080，所有网卡都监听。默认没有认证，测试期建议
#    按 docker-compose.yml 里的注释改成 "127.0.0.1:8080:8080"，让反代去对外，
#    或者按下面「开启客户端证书认证」那节配客户端证书。

# 2. 拿镜像。二选一：
#    a) 用 CI 推到 GHCR 的镜像（服务器上不用编译，推荐）
docker compose pull
#    b) 在服务器上自己构建（需要外网拉 golang:1.25-alpine 与依赖）
docker compose build

# 3. 先把正文语料建好，再起服务
docker compose run --rm web_ics --build-corpus

# 4. 起服务
docker compose up -d
docker compose logs -f
```

日志里应当出现这几行（体积是字节数除以 1024³，程序文案写作 `GB`，按 GiB 理解）：

```
发现 59 个文档包，共 11.5 GB，索引耗时 607ms
正文语料: /var/lib/web_ics-corpus（59/59 已就绪）
内存守卫: 软限 350 MB / 硬限 500 MB / 告警 450 MB
```

> 第 3 步不要省。正常启动路径是「后台建语料」与「首次搜索建标题索引」并发，
> 小内存机器会被拖进 swap 甚至被容器 OOM Kill。`--build-corpus` 只建语料，
> 不起 HTTP 服务也不起内存守卫，峰值最低。

验证：

```bash
curl -s http://127.0.0.1:8080/healthz                  # {"ok":true,"degraded":false,...}
curl -s http://127.0.0.1:8080/api/libs | head -c 300   # 59 条
docker compose exec web_ics /app/web_ics --selftest --doc-root /docs
docker stats --no-stream web_ics                       # 稳态约 190 MB，上限 500 MB
```

> 要放到公网有两条路：开[客户端证书认证](#开启客户端证书认证)，或者在反向代理上
> 加一层认证。默认配置没有任何认证，直接暴露等于把内部文档公开。

#### 二进制部署（服务器拉不到镜像时）

`docker compose build` 要拉 `golang:1.25-alpine` 与 `alpine:3.20` 两个基础镜像，
还要在容器里 `go mod download` 拉 `golang.org/x/text`。这些都拉不动时，
改走二进制部署，服务器上既不需要 Docker 也不需要 Go。

```bash
# 本机交叉编译。产物 7.3 MiB，已 -trimpath，不含构建机路径
bash ./build.sh
scp dist/web_ics-linux-amd64 <用户>@<服务器>:/tmp/

# 服务器
sudo mkdir -p /opt/web_ics /var/lib/web_ics-corpus
sudo mv /tmp/web_ics-linux-amd64 /opt/web_ics/web_ics
sudo chmod +x /opt/web_ics/web_ics
sudo chown -R "$USER":"$USER" /opt/web_ics /var/lib/web_ics-corpus

cd /opt/web_ics
# 先建语料，再起服务
./web_ics --build-corpus --doc-root /你的文档库目录 --corpus-dir /var/lib/web_ics-corpus
./web_ics --doc-root /你的文档库目录 --corpus-dir /var/lib/web_ics-corpus --addr :8080
```

单个二进制就够，前端资源编译在里面。想换界面再用 `WEB_ICS_WEB_ROOT` 指到外置目录，
它优先于内嵌资源。

#### 用 systemd 常驻

二进制部署要开机自启，存一份 `/etc/systemd/system/web_ics.service`：

```ini
[Unit]
Description=Web_ICS 产品文档阅读器
After=network.target

[Service]
Type=simple
User=web_ics
WorkingDirectory=/opt/web_ics
ExecStart=/opt/web_ics/web_ics \
    --doc-root /srv/ics-docs \
    --corpus-dir /var/lib/web_ics-corpus \
    --addr 127.0.0.1:8080
Restart=on-failure
RestartSec=3

# 整个文件系统只读，只放开语料目录。文档库只读挂载也能正常工作。
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
NoNewPrivileges=yes
ReadWritePaths=/var/lib/web_ics-corpus

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin web_ics
sudo chown -R web_ics:web_ics /var/lib/web_ics-corpus
sudo systemctl daemon-reload
sudo systemctl enable --now web_ics
journalctl -u web_ics -f          # 看启动日志
```

两处要按自己的环境改：

- `--doc-root` 指到文档库目录，并且 `web_ics` 这个用户要能读它。文档库如果放在
  `/home` 下，`ProtectHome=yes` 会把它挡住。那就把文档库挪到 `/srv` 之类的位置，
  或者去掉这一行。
- `--addr 127.0.0.1:8080` 是只听本机，由反向代理对外。直接写 `:8080` 会让所有网卡
  都能访问，而默认没有认证，不要这么用。要么开[客户端证书认证](#开启客户端证书认证)，
  要么让反向代理去认证。

#### 把项目传到服务器

项目代码不含文档库与语料库，排除后只有约 500 KB。

```bash
# 本机：打包。四个 --exclude 都不能省
tar czf web_ics-src.tgz --exclude=docs --exclude=dist --exclude=.git --exclude=.workbuddy-ai -C <本机项目目录> .
scp web_ics-src.tgz <用户>@<服务器>:/tmp/

# 服务器：解到部署目录
sudo mkdir -p /opt/web_ics
sudo tar xzf /tmp/web_ics-src.tgz -C /opt/web_ics
sudo chown -R "$USER":"$USER" /opt/web_ics
```

四个排除项各有理由：

- `docs/` 是本机建的语料库，1.8 GiB。服务器上会重建，传过去纯属浪费。
  它也在 `.dockerignore` 里，本来就不进镜像。
- `dist/` 是本机构建产物。容器里的二进制由 Dockerfile 自己编译。
- `.git/` 是版本历史，服务器上不需要。
- `.workbuddy-ai/` 是本地工具状态，已经在 `.gitignore` 里。

打包完先看体积，正常在 150 KB 上下。到了 GB 级说明 `docs/` 没排掉：

```bash
ls -lh web_ics-src.tgz
```

这些命令是 bash 语法（`/tmp/`、`$USER`、`<>` 占位符），要在 Git Bash 或 WSL 里跑。
cmd.exe 会把 `<` `>` 当成重定向符直接报错。

用 git 部署也可以，但 `git clone` 只会拿到已提交的版本。
先把改动提交并推送，再在服务器上 clone，否则拿到的是旧代码。

### 配置

#### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--config` | 自动查找 | 配置文件路径。不指定就依次找可执行文件旁边、用户配置目录，见[配置文件](#配置文件) |
| `--doc-root` | `./docs` | 文档库目录（含 `.hdx` / `.hwics`，只读一层） |
| `--corpus-dir` | `<doc-root>/.web_ics-corpus` | 正文语料目录（约 1.73 GiB，需可写） |
| `--build-corpus` | 关 | 只建正文语料后退出（不起 HTTP 服务） |
| `--addr` | 空 | 监听地址，如 `:8080`。留空则用 `--port` |
| `--port` | `8080` | 监听端口 |
| `--max-concurrency` | `10` | 最大在途请求数。2 核机器不要超过 16 |
| `--mem-limit` | `300` | Go 运行时软内存上限（MB），`0` 为不限制 |
| `--soft-limit` | `350` | 超过则清空目录树缓存与标题索引（MB） |
| `--panic-limit` | `450` | 超过则记告警，并开始拒绝新请求（503）（MB） |
| `--hard-limit` | `500` | 超过则记「硬限命中」计数（MB） |
| `--ip-rate` | `5` | 每 IP 每秒允许的请求数，`0` 关闭限流。只作用于 `/api/search` 与 `/api/grep` |
| `--ip-burst` | `20` | 每 IP 的突发容量 |
| `--trust-proxy` | `false` | 信任 `X-Forwarded-For` 作为客户端 IP。只有前面挂了反代才该开，否则任何人都能伪造这个头绕过限流 |
| `--debug-health` | `false` | 暴露 `/debug/healthz` 的详细指标（精确 RSS、缓存、在途请求） |
| `--client-ca` | 空 | 信任的客户端 CA 证书（PEM）。填了就要求客户端证书，服务改用 HTTPS。见[开启客户端证书认证](#开启客户端证书认证) |
| `--tls-cert` | 空 | 服务器证书（PEM）。配了 `--client-ca` 时必填 |
| `--tls-key` | 空 | 服务器私钥（PEM）。配了 `--client-ca` 时必填 |
| `--client-cert-deny` | 空 | 客户端证书吊销名单，每行一个 `sha256` 指纹 |
| `--open` | Windows 上开，其它平台关 | 启动后用系统默认浏览器打开界面。Windows 上默认开是因为那边是双击即用的桌面工具；Linux 上默认关，那是服务器。`--open=false` 可关 |
| `--selftest` | 关 | 运行内建自检后退出 |
| `--version` | 关 | 打印版本后退出 |

环境变量：`WEB_ICS_DOC_ROOT`、`WEB_ICS_CORPUS_DIR`、`WEB_ICS_ADDR`、`WEB_ICS_WEB_ROOT`、
`WEB_ICS_MAX_CONCURRENCY`、`WEB_ICS_IP_RATE`、`WEB_ICS_IP_BURST`、`WEB_ICS_TRUST_PROXY`、
`WEB_ICS_DEBUG_HEALTH`、`WEB_ICS_OPEN_BROWSER`、`WEB_ICS_TLS_CERT`、`WEB_ICS_TLS_KEY`、
`WEB_ICS_CLIENT_CA`、`WEB_ICS_CLIENT_CERT_DENY`、`GOMEMLIMIT`。

同一个设置可以在四个地方给，优先级是命令行参数 > 环境变量 > 配置文件 > 内置默认值。

#### 配置文件

不想每次都带一长串参数，就把它们写进 `web_ics.conf`。

程序启动时按顺序找：`--config` 指定的路径，可执行文件旁边的 `web_ics.conf`，
用户配置目录下的 `Web_ICS\web_ics.conf`（Windows 是 `%APPDATA%`，Linux 是 `~/.config`）。
一个都没找到就在可执行文件旁边生成一份带注释的模板，路径会打进启动日志。

格式是一行一个「键 = 值」，键名和命令行参数一样（去掉前面的 `--`）：

```
# 以 # 开头的是整行注释
doc-root = ./docs
corpus-dir = ./corpus

# 值里要用 # 就加引号，不加引号时「空白 + #」之后算行内注释
addr = 127.0.0.1:8080   # 只在本机访问
```

几个约定：

- 值可以加单引号或双引号，加了就原样取，里面可以有 `#` 和空格。
- 相对路径按配置文件所在目录解析，不是按工作目录。所以把 exe 和配置一起挪走，
  `doc-root = ./docs` 仍然指向它们旁边的 `docs`。
- 键名写错会直接报错并指出行号，不会静默忽略。
- 程序生成的模板里所有键都是注释状态，也就是什么都不改。要哪条就把 `#` 去掉。

> 阈值为什么定得这么高：冷启动工作集约 19 MB，建完标题索引约 290 MB。
> 阈值贴着稳态会让守卫每 5 秒清一次索引、每次搜索都要重建（约 5~8 秒）。
> 当前阈值下实测 `softLimitHits:0`、`hardLimitHits:0`。

> 已知不一致：`--panic-limit`（450）在实现里也会置为降级状态、拒绝新请求，
> 所以实际「开始拒绝」的阈值是 450 而不是 500。

#### 建语料库

语料库是正文全文检索的地基，一次性建好之后按包增量维护。

```bash
# 建完退出（首次部署，或文档包更新后跑一次）
web_ics --build-corpus --doc-root <文档库目录>

# 自定义语料目录（默认 <doc-root>/.web_ics-corpus）
web_ics --corpus-dir /var/lib/web_ics-corpus --doc-root <文档库目录>
```

- 正常启动时只做「缺失或过期」检查（59 次 `stat`，毫秒级），把重建丢到后台串行做，
  不阻塞启动。建好之前该包的正文不参与检索，响应里如实报告「语料 X/59」。
- 分片比包文件新就复用，文档包更新只重建那一个包。
- 语料目录必须可写，且不要放在只读挂载的文档库里。默认值就在文档库下面，
  文档库只读挂载时必须显式指定 `--corpus-dir`。

#### 开启客户端证书认证

默认情况下服务没有任何认证，谁连上谁就能看。配一个客户端 CA 之后，只有持有该 CA
签发的证书的客户端能访问。没有用户名密码这一步，也没有登录页与会话 cookie，
身份就是 TLS 握手本身，每次请求都验一遍。

自己做一套内部 CA 就够，不需要额外服务。下面这套命令在 Linux 与 Windows 的
Git Bash 里都能跑，两边都带 `openssl`。

第一步，建一个 CA。CA 私钥不要放到服务器上，签发都在自己机器上做：

```bash
mkdir -p certs && cd certs
openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.crt -days 3650 \
  -subj "/CN=我的文档站 CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"
```

第二步，签服务器证书。`subjectAltName` 要写客户端实际访问用的地址，现代浏览器
不看 CN：

```bash
cat > server.ext <<'EOF'
subjectAltName=DNS:docs.example.com,IP:192.168.1.10
extendedKeyUsage=serverAuth
keyUsage=digitalSignature
EOF

openssl req -newkey rsa:2048 -nodes -keyout server.key -out server.csr \
  -subj "/CN=docs.example.com"
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 365 -extfile server.ext
```

第三步，给每台要访问的设备签一张客户端证书。`extendedKeyUsage` 必须是
`clientAuth`，否则握手会被标准库拒掉：

```bash
cat > client.ext <<'EOF'
extendedKeyUsage=clientAuth
keyUsage=digitalSignature
EOF

openssl req -newkey rsa:2048 -nodes -keyout phone.key -out phone.csr \
  -subj "/CN=我的手机"
openssl x509 -req -in phone.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out phone.crt -days 365 -extfile client.ext
```

要装进浏览器还得打包成 PKCS#12：

```bash
openssl pkcs12 -export -out phone.pfx -inkey phone.key -in phone.crt \
  -passout pass:自己定一个密码
```

第四步，配置服务。三个键，`client-ca` 是开关，不写它就是普通的 HTTP：

```
client-ca = ./certs/ca.crt
tls-cert = ./certs/server.crt
tls-key = ./certs/server.key
```

相对路径按配置文件所在目录解析，所以 `./certs/` 指的是配置文件旁边那个 `certs`
目录。证书放在别处就写完整路径，比如 `client-ca = E:\ics-certs\ca.crt`。
找不到文件时启动日志会打出它实际找的那个路径，照着改就行。

`client-ca` 里可以放多张 CA 证书，轮换 CA 时新旧并存就能平滑过渡。配了
`client-ca` 却没配证书或私钥时服务直接拒绝启动，不会退化成「以为开了其实没开」。

第五步，把 CA 与客户端证书装进系统，两边都要装：

- CA 证书装进系统信任库，这样服务器证书不再报不受信任。Windows 双击 `ca.crt`，
  选「安装证书」，位置选「受信任的根证书颁发机构」。Linux 放进
  `/usr/local/share/ca-certificates/` 后跑 `update-ca-certificates`。
- 客户端证书双击 `phone.pfx` 导入，需要第三步设的密码。Windows 会放进当前用户的
  「个人」证书库。

Firefox 用自己的证书库，不读系统库，要在「设置 → 隐私与安全 → 证书 → 查看证书」
里再单独导入一次。

如果双击导入后浏览器弹出证书选择框、框里也有你的证书，但点「确定」没反应，两类
原因：一是服务端把连接掐了（本程序开启客户端证书认证时把 TLS 握手超时设为
20 秒，挑证书慢一点也不会被掐断，早期版本是 10 秒，容易中招）；二是私钥被装进了
老式的加密提供程序，用下面的命令重新导入。

先删掉旧的，再用 certutil 指定现代密钥存储导入：

```
certutil -user -delstore My <旧证书的 SHA-1 指纹>
certutil -user -csp KSP -p <pfx 密码> -importPFX My "<pfx 完整路径>" NoRoot,NoProtect
```

导入后 `certutil -user -store My` 里的「提供程序」应当是
`Microsoft Software Key Storage Provider`。指纹可以从同一条命令的输出里看，
也可以用 `openssl x509 -in phone.crt -noout -fingerprint -sha1`。

第六步，验证。浏览器打开服务地址会弹出证书选择框，选完就进去了。命令行可以用
`curl --cert phone.crt --key phone.key https://...`，或者
`openssl s_client -connect 主机:端口 -cert phone.crt -key phone.key -CAfile ca.crt`。

几个常见现象：

| 现象 | 原因 |
|---|---|
| 页面显示「需要客户端证书」 | 浏览器没有可用的证书。CA 与客户端证书都装了吗，Firefox 是不是漏了单独导入 |
| 浏览器报 `ERR_BAD_SSL_CLIENT_AUTH_CERT` | 证书不受信、已过期，或者用途不是 `clientAuth`。握手阶段就断了，服务端没有机会返回页面，具体原因在服务端日志里 |
| 页面显示「证书已被吊销」 | 这张证书在吊销名单里 |
| 浏览器一直不弹选择框 | 服务器证书本身不受信任时，有的浏览器会直接拒绝连接。先确认 CA 装好了 |
| 弹出了选择框、也能看到证书，但点「确定」没反应 | 按顺序查三件事。一是同一张证书在「当前用户」和「本地计算机」两个个人库里各装了一份，浏览器会挑到错的那张（`certutil -user -store My` 与 `certutil -store My` 各查一遍，同名的只留一张）；二是服务端把连接掐了（本程序在开启客户端证书认证时把 TLS 握手超时设为 20 秒，挑证书慢一点也不会被掐断）；三是浏览器本身的问题，实测 Edge 会出现这个现象，同一张证书同一个地址换成 Chrome 就正常 |

吊销一张证书：把它的 SHA-256 指纹写进名单，一行一个。

```bash
openssl x509 -in phone.crt -noout -fingerprint -sha256
# 输出形如 SHA256 Fingerprint=AB:CD:...，把冒号去掉、转小写填进去
```

```
# certs/deny.txt
sha256:abcdef... 我的旧手机
```

再配 `client-cert-deny = ./certs/deny.txt` 并重启。名单里有一行格式不对时服务会
拒绝启动，不会静默忽略某一条。

几件要知道的事：

- `/healthz` 也要证书，没有例外。探针与监控要么带证书，要么把探针跑在容器内部
  用同一份证书。留一个免认证的口子等于留一个能探测服务状态的入口。
- 服务一旦开启这个功能就只提供 HTTPS，明文请求会被拒。反向代理仍然可以放在前面，
  但那不再是必需的。
- 服务不做证书热加载，换证书之后要重启。
- 吊销名单也是启动时读一次，改完要重启。

### 使用

#### 界面

首页顶栏常驻搜索框，一个输入框同时搜标题和正文，没有「范围」下拉。
左侧是产品领域导航（带库数量），右侧是文档库表格。点左上角 `Web_ICS` 回首页。
列表没有复选框，本阅读器只读浏览，不提供批量管理。

阅读页顶栏是 `Web_ICS` 回首页、目录开关、面包屑、本文档内搜索、上下一篇、
回到顶部、内存指标。左侧目录树支持懒加载、拖动分栏、目录内筛选、全部折叠。
右侧是正文 iframe。

#### 搜索行为

| 页面 | 搜索框 | 搜的范围 | 请求 |
|---|---|---|---|
| 首页 | 搜索文档（标题 / 正文） | 全部 59 个文档包 | `/api/search?q=…` |
| 阅读页 | 在本文档中搜索 | 只有当前这个文档包 | `/api/search?q=…&lib=<libId>` |

两处走的是同一条检索链路，只差一个 `lib=` 参数。「搜全库 / 搜本文档」因此不是两种搜法，
而是同一搜法换个范围。

- 从搜索结果点进文档，落在阅读页而不是裸文档页
  （`/doc/{lib}/?url=…&node=…`）。这是刻意的：只有阅读页带「回首页」入口和
  本文档内搜索框，落在裸文档页会变成死胡同。深链参数会让目录树逐层展开到命中的那一篇
  并选中，面包屑同步显示所在章节。
- 阅读页的搜索结果覆盖在正文之上，不跳走。点某条直接在下方 iframe 里打开，
  目录树和当前位置都保留，关掉面板（或按 Esc）就能继续读原文。
- 结果多靠翻页解决，不截断。任何一页的 `total` 都是精确的全量命中数。
- 首页 100 条，后续每页 20 条。

### 发布与部署

三条链路，都由 GitHub Actions 驱动。

| 触发 | 工作流 | 做什么 |
|---|---|---|
| push 到 master，或推 `v*` tag | `release.yml` | 构建 Windows 免安装 exe 与 Linux 二进制，发布到 GitHub Releases |
| push 到 master | `deploy.yml` | 构建 Docker 镜像推到 GHCR（这一步总是跑）；再 SSH 到服务器 `docker compose pull && docker compose up -d`（默认关闭，见下面「自动部署要配的 secrets」） |
| 任意 push 与 PR | `ci.yml` | 格式、vet、交叉构建、构建机路径核对、依赖漏洞扫描、Dockerfile 可构建 |

#### 发一个版本

版本号只有一个来源：仓库根目录的 `VERSION` 文件。改那个文件里的数字，提交并 push 到
master，`release.yml` 就会发布 `v<那个数字>`，两个产物一起传上去。

```bash
# 把 VERSION 改成 0.2.0
git add VERSION && git commit -m "发 0.2.0" && git push
```

同一个版本号重复 push 不会重复建 Release，只把里面的产物换成最新的，标签也会重新指向
本次提交。要出新版本就再改一次 `VERSION`。

按标签发版也行，标签名优先于 `VERSION` 文件：

```bash
git tag v0.2.0
git push origin v0.2.0
```

带连字符的标签（如 `v0.2.0-rc1`）会标记成预发布。版本号最终由
`-ldflags -X main.version` 注入二进制，`--version` 打印的就是它。

#### 自动部署要配的 secrets

`deploy.yml` 里出镜像那一步不看服务器有没有配好，push 到 master 就会把镜像推到 GHCR，
打上 `latest`、`v<VERSION>`、`sha-<7位>` 三个标签。下面这些只是给「还要自动更新服务器」
用的，没有服务器就不用配。

仓库变量 `DEPLOY_ENABLED` 是部署的总开关：没设成 `true` 时 `deploy` 这个 job 显示为
「跳过」（灰色），push 代码不会去碰任何服务器。

准备好服务器之后，在仓库的 Settings → Secrets and variables → Actions 里加：

| Secret | 必填 | 说明 |
|---|---|---|
| `DEPLOY_HOST` | 是 | 服务器地址 |
| `DEPLOY_USER` | 是 | SSH 用户名 |
| `DEPLOY_SSH_KEY` | 是 | 私钥全文，含 `BEGIN` / `END` 行 |
| `DEPLOY_PORT` | 否 | SSH 端口，默认 22 |
| `DEPLOY_DIR` | 否 | 服务器上的 compose 目录，默认 `/opt/web_ics` |

再把同一页面 Variables 标签下的 `DEPLOY_ENABLED` 设成 `true`，部署就生效了。

服务器要能拉到镜像。GHCR 的包默认私有，二选一：把包改成 public，
或者先在服务器上执行一次 `docker login ghcr.io -u <用户名> -p <PAT>`。

推镜像用的是仓库自带的 `GITHUB_TOKEN`，`deploy.yml` 里已经给了 `packages: write`。
如果它仍被拒，那是 GHCR 的包级权限：到这个 package 的 Package settings →
Manage Actions access，把本仓库加成 Write。包已经存在（比如上次由别的仓库建过）
而名单里没有本仓库时就是这个症状。

开了总开关但 secrets 没配齐时，`deploy.yml` 会明确报出缺哪一项，
不会静默跳过。

---

## 整体结构

运行期只有三样东西：一个二进制、一个只读的文档库目录、一个可写的语料目录。
前端资源已经编进二进制，所以不需要旁边的 `web/` 目录。

```
cmd/web_ics/          入口、命令行解析、配置文件加载、内建自检
internal/doclib/      文档包索引（读 profile.xml）与 zip 流式读取、大小写折叠查找、产品领域规则
internal/nav/         目录树解析（navi.xml 优先，.hhc 兜底，FileList.xml 最后）+ 单包 LRU + 深链路径
internal/htmltext/    HTML 转纯文本（GBK 前置解码）、标题抽取、噪声路径判定
internal/textfold/    ASCII 大小写折叠，字节长度严格不变
internal/search/      标题索引（列式存储加字符串池）与融合检索、正文段两条路径
internal/corpus/      正文语料库：分片格式、建索引、全库扫描
internal/server/      HTTP 服务、路由、并发闸门、限流、安全头、流式直出
internal/memguard/    内存自守与 Go 运行时内存上限
web/                  前端（原生 HTML/CSS/JS，无框架），同时是把它 embed 进二进制的 Go 包
.github/workflows/    CI（ci.yml）、发布（release.yml）、自动部署（deploy.yml）
docs/                 正文语料库（默认位置，docs/.web_ics-corpus）
dist/                 构建产物（不进版本库）
```

各包的详细职责、关键类型、依赖方向与设计约束见
[DEVELOPMENT.md 的架构设计](DEVELOPMENT.md#架构设计)。

对外只有 10 条路由，完整参数、响应字段与语义见
[DEVELOPMENT.md 的接口参考](DEVELOPMENT.md#接口参考)：

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/` | 首页外壳 |
| GET | `/doc/{libId}/` | 阅读页外壳（深链参数 `?url=` / `?node=`） |
| GET | `/doc/{libId}/res/{包内路径}` | 正文 / 图片 / 脚本流式直出（大小写不敏感） |
| GET | `/api/libs` | 文档库列表（含产品领域） |
| GET | `/api/categories` | 产品领域分类及数量 |
| GET | `/api/nav` | 目录树按层返回，也支持深链定位 |
| GET | `/api/search` | 融合检索（标题与正文统一入口） |
| GET | `/api/grep` | 底层正文检索通道（脚本与排障用） |
| GET | `/healthz` | 健康检查（粗粒度，匿名可访问） |
| GET | `/debug/healthz` | 详细运行指标（默认关闭） |

只接受 `GET` 与 `HEAD`，其余方法返回 `405`。

---

## 运维

### 健康检查

| 路径 | 返回 | 用途 |
|---|---|---|
| `/healthz` | `{ok, degraded, memMB, memLevel}` | 给负载均衡与容器健康检查用，匿名可访问 |
| `/debug/healthz` | 完整指标 | 排障用，默认关闭，需要 `--debug-health` 才开 |

拆成两个入口的原因：匿名暴露实时 RSS、缓存命中率、在途请求数，等于给攻击者一把尺子，
能精确把负载压到内存降级阈值，让守卫反复清标题索引。`memMB` 因此是向上取整到
50 MB 倍数的近似值，`memLevel` 是 `normal` / `high` / `critical` 三档。
前端顶栏显示的就是这两个字段。

`/healthz` 在内存降级时返回 `ok:false`，但 HTTP 仍是 `200`。降级是正常的保护状态，
不该让容器重启。

### 内存策略

| 阈值 | 动作 |
|---|---|
| 软限 350 MB | 清空目录树缓存，释放标题索引（下次搜索重建，约 5~8 秒） |
| 告警 450 MB | 记告警日志，并开始拒绝新请求（503，带 `Retry-After`） |
| 硬限 500 MB | 记「硬限命中」计数 |
| 回到软限以下 | 解除降级 |

外加两层保护：Go 运行时 `GOMEMLIMIT=300MiB`（不设的话 Go 会按宿主机总内存决定 GC 时机，
一路涨到被容器 OOM Kill），以及容器 `memory: 500M` 硬限。
应用内先动，避免被容器杀。

### 内存统计口径

`/debug/healthz` 的 `rssBytes` 是进程工作集（Windows `psapi` 的 `workingSetSize`，
Linux `/proc/self/statm` 的常驻页乘页大小），也就是任务管理器、`ps aux` 的 RSS、
`docker stats` 那一层。它不是虚拟地址空间（实测同一时刻工作集 296 MB、
虚拟地址空间 5775 MB，差 19 倍），也不是累计分配量或峰值。
守卫每 5 秒采样一次，所以它最多滞后 5 秒。

### 磁盘占用

| 项 | 占用 |
|---|---|
| 文档库 | 11.5 GiB（只读挂载宿主机，零额外空间） |
| 语料库 | 1.73 GiB（可随时用 `--build-corpus` 重建） |
| 二进制 | 7.5 MiB（Windows）/ 7.3 MiB（Linux），`-s -w` 静态链接，含内嵌前端 |
| 镜像 | 约 50 MiB（Alpine 基础镜像） |

### 并发与限流

- 全局在途请求上限 10，超出直接 `503`。快速失败好过雪崩。
- 大文件（4 MiB 以上，如 PDF）单独限流，同时最多 2 个。
- 每 IP 每秒 5 次、突发 20，只作用于 `/api/search` 与 `/api/grep`。
  不限 `/api/nav` 与 `/static`：它们便宜，而且深链展开一棵几十层的目录树本来就要
  连发几十个 `/api/nav`，统一限流会把正常浏览挡掉。
- 配了反向代理后把 `--trust-proxy` 打开（或 `WEB_ICS_TRUST_PROXY=1`），
  否则所有请求看起来都来自反代那一个 IP，会互相挤占配额。

---

## 实测数据

本节是全项目性能与资源数字的唯一权威出处，另外两份文档只引用不重写。
测量环境：Windows，12 核（进程内 `GOMAXPROCS` 限为 2），59 个文档包 / 11.5 GiB，
语料库已建好（59/59）。测量日期 2026-09-30。

| 指标 | 实测 | 设计目标 |
|---|---|---|
| 二进制体积 | 7.5 MiB（Windows）/ 7.3 MiB（Linux） | < 15 MiB |
| 冷启动（扫描 59 包元数据） | 607 ms | < 3 s |
| 冷启动工作集（标题索引未建） | 约 19 MB | < 400 MB |
| 建完标题索引后的工作集 | 约 290 MB（峰值） | — |
| 稳态工作集（多次全库检索后） | 约 190 MB | < 400 MB |
| 标题索引规模 | 831,354 条 | — |
| 标题索引构建耗时（首次搜索时） | 4.6~8.0 s（随磁盘缓存冷热浮动） | — |
| 仅标题检索（热） | 60 ms | < 200 ms |
| 全文检索，全库 | 0.67~1.37 s（正文段 0.6~1.3 s） | — |
| 全文检索，单库（阅读页） | 77 ms（正文段 28 ms） | — |
| 正文 HTML 直出 | 5.5 ms | < 200 ms |
| 大目录首次展开（36,117 节点） | 186 ms | < 1 s |
| 同目录缓存后展开 | 2.3 ms | < 100 ms |
| 语料库规模 | 828,295 篇，正文 1.61 GiB，磁盘 1.73 GiB | — |
| 建语料耗时（一次性，59 包全量） | 4 分 16 秒 | — |
| 目录 URL 可解析率（含大小写回退） | 100%（741,924 条） | — |
| 正文内部引用可解析率 | 99.76%（846 条） | — |
| 内置自检 | 60 / 60 PASS | 全通过 |

几个代表性查询（全库，热查询）：

| 查询词 | total | 标题命中 | 正文命中 | 耗时 |
|---|---|---|---|---|
| `接口` | 277,473 | 6,941 主题 | 270,532 篇 | 1.06 s |
| `配置命令` | 190,802 | 982 | 189,820 | 0.67 s |
| `ip-add` | 34,001 | 231 | 33,770 | 1.37 s |
| `协议栈` | 2,709 | 27 | 2,682 | 0.93 s |

---

## 常见问题

### 首页正常但库列表一行都没有，或只显示表头

浏览器缓存了旧版前端资源。服务端已把 HTML 外壳设为 `no-store`、`/static/*` 设为
`no-cache`（走 `Last-Modified` / `ETag` 条件请求），正常不会再出现。若遇到，
先强刷（Ctrl+F5）确认。仍然复现就检查是否有中间层缓存。改了前端资源记得把
`web/*.html` 里的 `?v=N` 一起加一。

### 有些文档的文档版本是空的

产品版本与日期一般都能读到。它们在包里的位置有两种：包根 `profile.xml`，或者
`resources/infocenter_service/profile.xml`，两种都认。

文档版本为空的那几个是 HedEx 2.0 导出的 `.hwics`，包里的 `resourcelibversion`
字段本身就是空的。鼠标停在那几个格子上会提示「该文档包未提供文档版本」。
这是包没带这个信息，不是解析失败，产品版本与日期能正常显示。

### 正文中文乱码

正文 HTML 声明 `charset=gb2312` 而字节是 GBK，Go 的 `mime.TypeByExtension(".html")`
返回 `text/html; charset=utf-8`，HTTP 头的 charset 优先级高于 HTML 内的 `<meta>`，
浏览器会按 UTF-8 解码 GBK 字节。服务端已对文本类型一律不带 charset。
用 `curl -D - -o NUL` 检查 `Content-Type`，出现 `charset=utf-8` 就是这条失效了。

### 正文里的标签页（报文示例 / 参考标准 / 协议栈结构）点了没反应

包内的 `tabSection.js` / `customQuery.js` 请求 404 导致的。根因是文档里 URL 的
大小写与包内文件名不一致（驼峰对全小写）。`Lookup` 已做小写折叠回退，用

```bash
curl -s -o /dev/null -w '%{http_code}\n' \
  "$BASE/doc/<lib>/res/public_sys-resources/tabSection.js"
```

验证，应当是 `200` 而不是 `404`。

### 搜索很慢或每次搜索都重建索引

看 `/debug/healthz` 的 `softLimitHits`。大于 0 说明内存守卫在反复释放标题索引，
每次搜索都要重建（4.6~8.0 s）。把 `--soft-limit` 调高，或确认 `--mem-limit` /
`GOMEMLIMIT` 是否设得过低。

### 搜索报「无法连接服务」

前端已把 `Failed to fetch` 翻译成人话并区分「用户主动取消」。看到这条说明请求
真的没发出去：确认服务进程还在（命令行窗口没关）、端口没变、页面不是过期标签页。
命令行用 `curl --noproxy '*' http://127.0.0.1:8080/healthz` 先确认服务端好着。

### 浏览器转圈打不开，命令行却正常

本机代理会劫持 localhost。把 `127.0.0.1;localhost` 加进系统代理的绕过列表，
命令行加 `--noproxy '*'`。

### 正文检索提示「已就绪 X/59」

语料库还在后台建立（约 4 分钟）。建好之前那些包的正文不参与检索，这是如实报告，
不是结果被丢弃。想一次建好就先跑 `--build-corpus`。

---

## 它不做什么

按「不做多余的额外处理」原则，以下原版功能刻意未实现：

- 倒排索引与中文分词（原版用 Lucene，这里换成磁盘语料库加全库顺序扫）
- 批注与笔记（`docnote`）
- Office 在线预览与导出（原版最大的重依赖，且这批文档里 Office 附件几乎为 0）
- 打印导出、版本对比、下载任务管理、虚拟文件夹
- 用户体系（用户名密码、角色、按人授权）、隐私声明（原版那串 filter 是给企业内网
  合规用的）。访问控制只做客户端证书这一种，做了就不配证书等于没开，见
  [开启客户端证书认证](#开启客户端证书认证)
- 任何写操作。服务是只读的，没有写接口

## 相关文档

- [DEVELOPMENT.md](DEVELOPMENT.md)：开发环境、架构设计、编码规范、构建与测试流程、协作约定
- [PLAN.md](docs/PLAN.md)：立项时的背景、目标、调研、预期范围与初步规划

## 许可证

MIT，全文见 [LICENSE](LICENSE)。

唯一的外部依赖 `golang.org/x/text` 是 BSD-3-Clause，通过 Go module 引用，
没有 vendored 进仓库。

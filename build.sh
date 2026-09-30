#!/usr/bin/env bash
# 构建交付产物。
#
# 为什么不直接 go build：交付的二进制必须满足两件事。
#
# 1) 带 -trimpath。不带的话二进制里会留下构建机上的绝对路径（源码目录与
#    go/pkg/mod 模块缓存目录），交付出去等于把构建环境告诉别人。
#    脚本最后会自己 grep 一遍确认干净。
#
# 2) 工具链必须是 Go 1.25+。go1.23.4 的标准库有 34 条已公开漏洞
#    （govulncheck 判定可达，其中 4 条落在 HTTP 请求处理路径上）。
#    换到 go1.25.13 之后复扫是 0 条。
#
# 用法：
#   ./build.sh                      # 自动挑可用的新工具链
#   GO=go1.25.13 ./build.sh         # 指定
set -euo pipefail
cd "$(dirname "$0")"

# 优先用新工具链：装过 go1.25.13 就自动用它，避免误用旧版出交付物
if [ -z "${GO:-}" ]; then
  for cand in go1.25.13 go1.25 go; do
    if command -v "$cand" >/dev/null 2>&1; then GO="$cand"; break; fi
  done
fi

echo "==> 工具链: $("$GO" version)"
ver=$("$GO" version | sed -n 's/.* go1\.\([0-9]*\).*/\1/p')
if [ -z "$ver" ] || [ "$ver" -lt 25 ]; then
  cat <<'EOF'

!! 当前工具链的 Go 标准库有已公开漏洞，不能用来出交付产物。
   go1.23.4 上 govulncheck 报 34 条可达漏洞，其中 4 条在 HTTP 请求路径上。

   装一个 1.25 的工具链（只下载一次，装在 ~/sdk，不影响你现有的 Go）：

     go install golang.org/dl/go1.25.13@latest
     go1.25.13 download

   然后重跑 ./build.sh 即可，脚本会自动挑它。

   确实要用旧工具链出包（不建议）：GO=go ./build.sh

EOF
  exit 1
fi

# 版本号注入：CI 传 VERSION（取自 tag），本地则取最近的 git tag，
# 都没有时退回 0.1.0。它决定 --version 打印出来的值。
#
# 注意不能写成 ${VERSION:-$(git describe ...)}：仓库一个 tag 都没有时
# git describe 返回 128，配合 set -e 与 pipefail 会把脚本直接杀掉。
VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION=$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//') || true
fi
[ -n "$VERSION" ] || VERSION="0.1.0"
echo "==> 版本号: $VERSION"

LDFLAGS="-s -w -X main.version=$VERSION"
mkdir -p dist

# go build 默认会读 git 状态，把提交信息写进二进制。.git 损坏时（比如被清空、
# 只剩一个空目录）它会报 "error obtaining VCS status: exit status 128" 然后
# 直接失败，那句报错完全看不出跟 git 有关。这里先探一下，坏了就跳过 VCS 标记
# 让构建继续，并说明原因。.git 整个不存在时 go 本来就会跳过，不用管。
if [ -e .git ] && ! git rev-parse --git-dir >/dev/null 2>&1; then
  echo "==> 警告：.git 存在但不可用，跳过 VCS 标记"
  export GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false"
fi

echo "==> 检查格式与静态问题"
gofmt -l ./cmd ./internal ./web | tee /tmp/gofmt.out
if [ -s /tmp/gofmt.out ]; then
  echo "有文件没格式化，先跑 gofmt -w ./cmd ./internal ./web"
  exit 1
fi
"$GO" vet ./...

echo "==> Windows amd64"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  "$GO" build -trimpath -ldflags="$LDFLAGS" -o dist/web_ics.exe ./cmd/web_ics

echo "==> Linux amd64"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  "$GO" build -trimpath -ldflags="$LDFLAGS" -o dist/web_ics-linux-amd64 ./cmd/web_ics

echo "==> 核对：二进制里不该出现构建机路径"
# 正则要求盘符前面不是字母，后面紧跟字母或下划线，否则 "http://" 里的
# "p:/" 会被误判成盘符路径。
BAD='[^A-Za-z0-9][A-Za-z]:/[A-Za-z0-9_][A-Za-z0-9_./-]{4,}'
for f in dist/web_ics.exe dist/web_ics-linux-amd64; do
  # grep 没命中时返回 1，配合 set -e + pipefail 会直接退出，所以显式吞掉
  hits=$(grep -aoE "$BAD" "$f" 2>/dev/null | sort -u | head -5 || true)
  if [ -n "$hits" ]; then
    echo "!! $f 里仍有绝对路径："
    echo "$hits"
    exit 1
  fi
  if grep -aq 'go/pkg/mod' "$f"; then
    echo "!! $f 里仍有模块缓存路径"
    exit 1
  fi
done
echo "    干净"

echo
ls -lh dist/
echo "完成。"

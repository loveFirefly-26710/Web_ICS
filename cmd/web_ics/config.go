package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 配置文件解决的是「每次启动都要带一长串参数」这件事，最常见的需求就是把
// 文档库目录固定下来。
//
// 格式是 key = value 的纯文本，不用 JSON：这里最常填的是 Windows 路径，
// JSON 要求把每个反斜杠写成两个，抄一次错一次。也不用 TOML，那要引新依赖，
// 而本项目的依赖只有一个 golang.org/x/text（GBK 解码），是必要的那种。
//
// 优先级：命令行 > 环境变量 > 配置文件 > 内置默认。

const configName = "web_ics.conf"

// setting 描述一个可以写进配置文件的参数。
type setting struct {
	name   string // 命令行名，同时也是配置文件里的键名
	env    string // 对应的环境变量名，空表示没有
	kind   string // string / int / float / bool
	isPath bool   // 相对路径要按配置文件所在目录解析
}

// settings 是允许出现在配置文件里的键。动作类参数（version / selftest /
// build-corpus / config）不在其中：它们表达「这次要做什么」，不是配置。
var settings = []setting{
	{"doc-root", "WEB_ICS_DOC_ROOT", "string", true},
	{"corpus-dir", "WEB_ICS_CORPUS_DIR", "string", true},
	{"addr", "WEB_ICS_ADDR", "string", false},
	{"port", "", "int", false},
	{"max-concurrency", "WEB_ICS_MAX_CONCURRENCY", "int", false},
	{"mem-limit", "", "int", false},
	{"soft-limit", "", "int", false},
	{"panic-limit", "", "int", false},
	{"hard-limit", "", "int", false},
	{"ip-rate", "WEB_ICS_IP_RATE", "float", false},
	{"ip-burst", "WEB_ICS_IP_BURST", "int", false},
	{"trust-proxy", "WEB_ICS_TRUST_PROXY", "bool", false},
	{"debug-health", "WEB_ICS_DEBUG_HEALTH", "bool", false},
	{"open", "WEB_ICS_OPEN_BROWSER", "bool", false},
}

// configFile 是一次配置加载的结果。
type configFile struct {
	Path     string            // 实际用上的文件路径，空表示一个都没找到
	Vals     map[string]string // 键到原始值
	Created  bool              // 文件是这次启动刚生成的（里面只有注释，没有生效的值）
	Searched []string          // 找过哪些位置，没找到时用来提示
}

// loadConfig 按顺序找一个配置文件并解析。
//
// 显式指定 --config 时只用它，读不到直接报错：打错路径还静默退回默认值，
// 用户会以为「我配了」而实际没生效。
//
// 没显式指定时，先看可执行文件旁边（便携用法，拷到哪带到哪），
// 再看用户配置目录（安装版，exe 目录可能不可写）。一个都没有就生成一份模板，
// 免得用户不知道这个文件该长什么样、该放哪里。
func loadConfig(explicit string) *configFile {
	cf := &configFile{Vals: map[string]string{}}

	if explicit != "" {
		explicit = absPath(explicit)
		vals, err := readConfig(explicit)
		if err != nil {
			log.Fatalf("读 --config 指定的配置文件失败: %v", err)
		}
		cf.Path, cf.Vals, cf.Searched = explicit, vals, []string{explicit}
		return cf
	}

	cands := configCandidates()
	cf.Searched = cands
	for _, p := range cands {
		vals, err := readConfig(p)
		if err != nil {
			// 文件不存在是最常见的情况，继续看下一个；文件存在但读不了
			// 也照样跳过，最后的提示里会列出找过哪些位置。
			continue
		}
		cf.Path, cf.Vals = p, vals
		return cf
	}

	for _, p := range cands {
		if err := writeConfigTemplate(p); err == nil {
			cf.Path, cf.Created = p, true
			return cf
		}
	}
	return cf
}

// absPath 把配置路径转成绝对路径。
//
// 必须转：配置文件里的相对路径是按配置文件所在目录解析的，而路径本身是相对的
// 时候 filepath.Dir 只会得到 "."，解析就退化成相对工作目录，静默失效。
// 顺带让日志里的路径可以直接复制去用。
func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// configCandidates 返回按优先级排列的候选路径。
func configCandidates() []string {
	var out []string
	// 用 os.Executable 而不是 os.Getwd：双击启动时两者一样，但从别的工作目录
	// 启动时不一样，而「配置放在 exe 旁边」这个约定必须按 exe 算。
	if p, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(p), configName))
	}
	if d, err := os.UserConfigDir(); err == nil {
		out = append(out, filepath.Join(d, "Web_ICS", configName))
	}
	return out
}

// readConfig 读并解析一个配置文件。
func readConfig(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vals, err := parseConfig(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return vals, nil
}

// parseConfig 解析 key = value 形式的配置。
//
// 规则：
//   - 以 # 开头的整行是注释，空行忽略
//   - 键与值用第一个 = 分隔，两侧空白去掉
//   - 值可以加单引号或双引号，加了引号就原样取值，里面可以有 # 和空格
//   - 不加引号时，值里第一个「空白 + #」之后算行内注释
//   - 同一个键写两次，后面的覆盖前面的
//   - 键不认识直接报错，不静默忽略：打错一个字母不该变成「配了但没生效」
func parseConfig(text string) (map[string]string, error) {
	known := make(map[string]bool, len(settings))
	for _, s := range settings {
		known[s.name] = true
	}

	vals := make(map[string]string)
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("第 %d 行缺少等号: %s", i+1, line)
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return nil, fmt.Errorf("第 %d 行等号左边是空的", i+1)
		}
		val, err := trimValue(line[eq+1:])
		if err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", i+1, err)
		}
		if !known[key] {
			return nil, fmt.Errorf("第 %d 行有不认识的键 %q", i+1, key)
		}
		vals[key] = val
	}
	return vals, nil
}

// trimValue 去掉值两侧的空白与行内注释。
func trimValue(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if q := s[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(s[1:], q)
		if end < 0 {
			return "", fmt.Errorf("引号没有闭合: %s", s)
		}
		return s[1 : 1+end], nil
	}
	// 只在「空白 + #」或行首是 # 时截断，这样路径里的 # 不会被误切。
	// 值真的要带 # 就加引号，上面那条分支会原样返回。
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			s = s[:i]
			break
		}
	}
	return strings.TrimSpace(s), nil
}

// normalize 按类型校验并规范化一个值。返回的字符串直接喂给 flag.Set。
//
// 先去掉两侧空白：配置文件里的值已经被 trimValue 处理过，但环境变量没有，
// WEB_ICS_MAX_CONCURRENCY=" 10 " 这种写法不该失败。
func normalize(kind, v string) (string, error) {
	v = strings.TrimSpace(v)
	switch kind {
	case "int":
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("要整数，拿到 %q", v)
		}
		return strconv.Itoa(n), nil
	case "float":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return "", fmt.Errorf("要数字，拿到 %q", v)
		}
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case "bool":
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return "true", nil
		case "0", "false", "no", "off":
			return "false", nil
		}
		return "", fmt.Errorf("要布尔值（true/false/1/0/yes/no/on/off），拿到 %q", v)
	}
	return v, nil
}

// applySettings 按优先级把环境变量与配置文件的值填进 flag。
//
// 所有 flag 的默认值填的都是内置默认，所以这里只需要处理命令行没显式指定的
// 那些。必须在 flag.Parse 之后、读任何 flag 值之前调用。
func applySettings(cf *configFile) {
	explicit := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	for _, s := range settings {
		if explicit[s.name] {
			continue
		}
		if s.env != "" {
			if v := os.Getenv(s.env); v != "" {
				mustSet(s, v, "环境变量 "+s.env, "")
				continue
			}
		}
		if v, ok := cf.Vals[s.name]; ok {
			base := ""
			if cf.Path != "" {
				base = filepath.Dir(cf.Path)
			}
			mustSet(s, v, "配置文件 "+cf.Path, base)
		}
	}
}

// mustSet 校验一个值并写进 flag，失败就退出。
//
// relBase 非空且这个键是路径时，相对路径按它解析：把 exe 和配置一起挪走之后
// doc-root = ./docs 仍然指向 exe 旁边，不受启动时工作目录影响。
func mustSet(s setting, raw, from, relBase string) {
	norm, err := normalize(s.kind, raw)
	if err != nil {
		log.Fatalf("%s 里的 %s 不对: %v", from, s.name, err)
	}
	if s.isPath && relBase != "" && !filepath.IsAbs(norm) {
		norm = filepath.Join(relBase, norm)
	}
	if err := flag.Set(s.name, norm); err != nil {
		log.Fatalf("%s 里的 %s 无效: %v", from, s.name, err)
	}
}

// logSummary 说明这次配置是从哪来的，方便排查「我配了怎么没生效」。
func (cf *configFile) logSummary() {
	switch {
	case cf.Created:
		log.Printf("首次运行，已生成配置文件: %s", cf.Path)
		log.Printf("文档库目录等设置写在这个文件里，改完重启生效")
	case cf.Path != "":
		log.Printf("配置文件: %s", cf.Path)
	default:
		log.Printf("没有配置文件，用内置默认值启动（找过 %s）", strings.Join(cf.Searched, " 和 "))
	}
}

// describe 给自检用的一句话描述。
func (cf *configFile) describe() string {
	switch {
	case cf.Created:
		return cf.Path + "（本次刚生成）"
	case cf.Path != "":
		return cf.Path
	}
	return "无（找过 " + strings.Join(cf.Searched, " 和 ") + "）"
}

// writeConfigTemplate 写一份带注释的配置模板。
//
// 已存在就不动，免得把用户的配置覆盖掉（比如文件存在但当时读不了）。
func writeConfigTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("文件已存在")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(configTemplate), 0o644)
}

const configTemplate = `# Web_ICS 配置文件
#
# 程序首次启动时会生成一份，放在可执行文件旁边；那里不可写就放到用户配置目录。
# 改完重启程序生效。
#
# 格式是一行一个「键 = 值」。以 # 开头的整行是注释；值里要用 # 就给值加引号。
# 相对路径按本文件所在目录解析，所以把 exe 和这个文件一起挪走也没关系。
#
# 优先级：命令行参数 > 环境变量 > 本文件 > 内置默认值。
#
# 下面每一条都是注释状态，也就是什么都不改。程序生成这个文件不会改变任何行为，
# 要用哪条就把前面的 # 去掉。

# 文档包（.hdx / .hwics）所在的目录。只扫这一层、不递归，59 个包要直接放在里面。
# 不写就是启动时工作目录下的 docs（双击启动时工作目录就是本文件所在目录）。
# 想固定下来就用下面这个，把文档包放进本文件旁边的 docs 目录：
# doc-root = ./docs
# 文档包已经在别处（比如某个盘的数据目录）就填那个目录的完整路径。

# 正文语料目录。默认是 <doc-root>/.web_ics-corpus，必须可写。
# 文档库是只读的时候把它指到别处，比如本文件旁边的 corpus。
# corpus-dir = ./corpus

# 监听地址与端口。addr 写了就忽略 port。
# addr = 127.0.0.1:8080
# port = 8080

# 下面这些一般不用改，完整说明见 README 的参数表。
# max-concurrency = 10
# mem-limit = 300
# soft-limit = 350
# panic-limit = 450
# hard-limit = 500
# ip-rate = 5
# ip-burst = 20
# trust-proxy = false
# debug-health = false
# open = true
`

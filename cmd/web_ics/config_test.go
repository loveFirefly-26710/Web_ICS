package main

import (
	"flag"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr string // 非空表示期望报错，值是要能在错误里找到的片段
	}{
		{
			name: "普通键值",
			in:   "doc-root = /srv/docs\n",
			want: map[string]string{"doc-root": "/srv/docs"},
		},
		{
			name: "等号两侧没有空格",
			in:   "port=8080\n",
			want: map[string]string{"port": "8080"},
		},
		{
			name: "等号两侧多余空白",
			in:   "   port   =   8080   \n",
			want: map[string]string{"port": "8080"},
		},
		{
			name: "整行注释与空行",
			in:   "# 说明\n\n   \nport = 8080\n# 结尾注释\n",
			want: map[string]string{"port": "8080"},
		},
		{
			name: "行内注释",
			in:   "port = 8080  # 监听端口\n",
			want: map[string]string{"port": "8080"},
		},
		{
			name: "行内注释用制表符分隔",
			in:   "port = 8080\t# 注释\n",
			want: map[string]string{"port": "8080"},
		},
		{
			name: "路径里的井号不当注释",
			in:   `doc-root = C:\a#b` + "\n",
			want: map[string]string{"doc-root": `C:\a#b`},
		},
		{
			name: "Windows 路径的反斜杠原样保留",
			in:   `doc-root = C:\文档库\产品文档` + "\n",
			want: map[string]string{"doc-root": `C:\文档库\产品文档`},
		},
		{
			name: "双引号里的井号与空格",
			in:   `doc-root = "C:\a # b\docs"  # 真的注释` + "\n",
			want: map[string]string{"doc-root": `C:\a # b\docs`},
		},
		{
			name: "单引号",
			in:   "doc-root = 'C:\\a b'\n",
			want: map[string]string{"doc-root": `C:\a b`},
		},
		{
			name: "值里再有等号",
			in:   "doc-root = C:\\a=b\n",
			want: map[string]string{"doc-root": `C:\a=b`},
		},
		{
			name: "空值",
			in:   "addr =\n",
			want: map[string]string{"addr": ""},
		},
		{
			name: "空值只留注释",
			in:   "addr =   # 空\n",
			want: map[string]string{"addr": ""},
		},
		{
			name: "同一个键写两次后面覆盖前面",
			in:   "port = 1\nport = 2\n",
			want: map[string]string{"port": "2"},
		},
		{
			name: "CRLF 换行",
			in:   "port = 8080\r\ndoc-root = /d\r\n",
			want: map[string]string{"port": "8080", "doc-root": "/d"},
		},
		{
			name: "多个键",
			in:   "doc-root = /d\nport = 9\nopen = true\n",
			want: map[string]string{"doc-root": "/d", "port": "9", "open": "true"},
		},
		{
			name:    "不认识的键要报错",
			in:      "docroot = /d\n",
			wantErr: `不认识的键 "docroot"`,
		},
		{
			name:    "缺等号要报错",
			in:      "doc-root /d\n",
			wantErr: "缺少等号",
		},
		{
			name:    "等号左边为空要报错",
			in:      " = /d\n",
			wantErr: "等号左边是空的",
		},
		{
			name:    "引号没闭合要报错",
			in:      `doc-root = "C:\a` + "\n",
			wantErr: "引号没有闭合",
		},
		{
			name:    "报错要带行号",
			in:      "port = 8080\ndocroot = /d\n",
			wantErr: "第 2 行",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseConfig(c.in)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错（含 %q），实际通过了，结果 %v", c.wantErr, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("错误信息里没有 %q: %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("键数不对，得到 %v，期望 %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("%s: 得到 %q，期望 %q", k, got[k], v)
				}
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		kind    string
		in      string
		want    string
		wantErr bool
	}{
		{"string", "C:\\a b", "C:\\a b", false},
		{"int", "8080", "8080", false},
		{"int", " 8080 ", "8080", false},
		{"int", "80.5", "", true},
		{"int", "abc", "", true},
		{"float", "5", "5", false},
		{"float", "0.5", "0.5", false},
		{"float", "x", "", true},
		{"bool", "true", "true", false},
		{"bool", "TRUE", "true", false},
		{"bool", "yes", "true", false},
		{"bool", "on", "true", false},
		{"bool", "1", "true", false},
		{"bool", "false", "false", false},
		{"bool", "no", "false", false},
		{"bool", "0", "false", false},
		{"bool", "maybe", "", true},
	}

	for _, c := range cases {
		got, err := normalize(c.kind, c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalize(%s, %q) 期望报错，得到 %q", c.kind, c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalize(%s, %q) 不该报错: %v", c.kind, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalize(%s, %q) = %q，期望 %q", c.kind, c.in, got, c.want)
		}
	}
}

// 模板必须能被自己的解析器读懂：否则首次运行生成的文件会立刻让程序启动失败。
//
// 而且模板里不能有任何生效的键。生成配置文件这个动作本身不该改变程序行为，
// 否则用户什么都没改就发现行为和以前不一样了。
func TestConfigTemplateIsParseableAndInert(t *testing.T) {
	vals, err := parseConfig(configTemplate)
	if err != nil {
		t.Fatalf("模板解析失败: %v", err)
	}
	if len(vals) != 0 {
		t.Fatalf("模板里不该有生效的键，实际有 %v", vals)
	}
}

// 配置表里的 kind 必须是 normalize 认识的那几种。写错了 normalize 会原样返回，
// 于是要到启动时才在 flag.Set 那一步炸，报错也看不出是 kind 写错了。
// 这里用一份临时 FlagSet 把每个键按声明的类型注册一遍，类型不认识就会注册失败。
func TestSettingsKindsAreKnown(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	seen := make(map[string]bool)
	for _, s := range settings {
		if seen[s.name] {
			t.Errorf("settings 里 %q 出现了两次", s.name)
		}
		seen[s.name] = true
		switch s.kind {
		case "string":
			fs.String(s.name, "", "")
		case "int":
			fs.Int(s.name, 0, "")
		case "float":
			fs.Float64(s.name, 0, "")
		case "bool":
			fs.Bool(s.name, false, "")
		default:
			t.Errorf("%s 的 kind %q 不认识", s.name, s.kind)
		}
	}
}

// 每个 kind 都得有能通过的值，否则配置表里某一类键永远填不进去。
func TestEveryKindRoundTrips(t *testing.T) {
	sample := map[string]string{
		"string": "/d",
		"int":    "1",
		"float":  "1.5",
		"bool":   "true",
	}
	for _, s := range settings {
		v, ok := sample[s.kind]
		if !ok {
			t.Fatalf("%s 的 kind %q 没有样例值", s.name, s.kind)
		}
		if _, err := normalize(s.kind, v); err != nil {
			t.Errorf("normalize(%s, %q) 失败: %v", s.kind, v, err)
		}
	}
}

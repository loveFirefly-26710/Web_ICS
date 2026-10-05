package doclib

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// makePkg 造一个只含指定条目的包文件，返回它的路径。
func makePkg(t *testing.T, name string, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("建包失败: %v", err)
	}
	zw := zip.NewWriter(f)
	for n, body := range entries {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatalf("写条目 %s 失败: %v", n, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("写条目 %s 内容失败: %v", n, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关 zip 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关文件失败: %v", err)
	}
	return p
}

// .hdx 的 profile.xml：在包根，驼峰标签。片段取自真实的 AZM1017P 包。
const profileCamel = `<?xml version="1.0" encoding="UTF-8"?>
<profile>
  <libId>AZM1017P</libId>
  <libVersion>05</libVersion>
  <libName>S300, S500, S2700, S5700, S6700 产品文档</libName>
  <productType>S300, S500, S2700, S5700, S6700</productType>
  <productVersion>V200R023C00</productVersion>
  <issueDate>2024-11-10</issueDate>
  <language>zh</language>
  <topicNumber>26081</topicNumber>
  <guid>F37EFF80-BFFC-4383-9A35-DFF226F2D94F</guid>
</profile>`

// HedEx 2.0 的 .hwics：只有 resources/infocenter_service/profile.xml，
// 标签全小写、名字也不同，而且 resourcelibversion 是空的。
// 这一段是 NE9000 那个包的原样内容。
const profileHedex = `<?xml version="1.0" encoding="UTF-8"?>
<profile><productid>prodname_94_6587</productid><productname>NE9000</productname><apptype></apptype><productversionid>release_2165</productversionid><productversion>V800R021C00</productversion><projectid>934037318</projectid><resourcelibname>V800R021C00 产品文档_new</resourcelibname><language>zh</language><resourcelibversion></resourcelibversion><issuedate>2022-04-13 03:05:52</issuedate><srctype>0</srctype><updatetype>0</updatetype><guid>0</guid><owner></owner><hdx>0</hdx><libid>ICS2af45d098</libid><upgradetype>1</upgradetype></profile>`

// 驼峰那份：所有字段都该读到。
func TestReadMetaCamelSchema(t *testing.T) {
	p := makePkg(t, "某产品_V200R023C00_05_zh_AZM1017P.hdx",
		map[string]string{"profile.xml": profileCamel, "resources/navi.xml": "<navi/>"})

	m, err := readMeta(p, filepath.Base(p))
	if err != nil {
		t.Fatalf("readMeta 失败: %v", err)
	}
	want := LibMeta{
		LibID:          "AZM1017P",
		LibName:        "S300, S500, S2700, S5700, S6700 产品文档",
		ProductType:    "S300, S500, S2700, S5700, S6700",
		ProductVersion: "V200R023C00",
		LibVersion:     "05",
		IssueDate:      "2024-11-10",
		Language:       "zh",
		TopicNumber:    26081,
		Guid:           "F37EFF80-BFFC-4383-9A35-DFF226F2D94F",
		HasNaviXML:     true,
	}
	got := *m
	got.FilePath, got.FileName, got.SizeBytes = "", "", 0
	if got != want {
		t.Fatalf("字段不对\n得到 %+v\n期望 %+v", got, want)
	}
}

// HedEx 那份：位置不在包根，标签全小写。版本号与日期必须能读到，
// 这是「部分文档版本号读不出来」那个问题的回归防线。
func TestReadMetaHedexSchema(t *testing.T) {
	const fileName = "NE9000 V800R021C00 产品文档.hwics"
	p := makePkg(t, fileName, map[string]string{
		"resources/infocenter_service/profile.xml": profileHedex,
		"resources/FileList.xml":                   "<fileList/>",
	})

	m, err := readMeta(p, fileName)
	if err != nil {
		t.Fatalf("readMeta 失败: %v", err)
	}
	if m.ProductVersion != "V800R021C00" {
		t.Errorf("ProductVersion = %q，期望 V800R021C00", m.ProductVersion)
	}
	if m.IssueDate != "2022-04-13" {
		t.Errorf("IssueDate = %q，期望 2022-04-13（包内带时间，要截掉）", m.IssueDate)
	}
	if m.ProductType != "NE9000" {
		t.Errorf("ProductType = %q，期望 NE9000", m.ProductType)
	}
	if m.LibID != "ICS2af45d098" {
		t.Errorf("LibID = %q，期望 ICS2af45d098", m.LibID)
	}
	// 包里的 resourcelibversion 是空的，文档版本本来就拿不到，只能空着。
	if m.LibVersion != "" {
		t.Errorf("LibVersion = %q，期望空（包内 resourcelibversion 是空的）", m.LibVersion)
	}
	// resourcelibname 比文件名差，不该被采纳。
	if m.LibName != "NE9000 V800R021C00 产品文档" {
		t.Errorf("LibName = %q，期望回退到文件名", m.LibName)
	}
	if m.HasNaviXML {
		t.Error("这个包没有 resources/navi.xml，HasNaviXML 不该为真")
	}
}

// 两份都有时取包根那份：它信息更全（有 libVersion）。
func TestReadMetaPrefersRootProfile(t *testing.T) {
	const fileName = "SD-WAN V100R021C00 产品文档（基于iMaster NCE-Campus）.hwics"
	root := `<?xml version="1.0" encoding="UTF-8"?>
<profile>
  <libId>356923456_CN</libId>
  <libVersion>01</libVersion>
  <libName>SD-WAN V100R021C00 产品文档（基于iMaster NCE-Campus）</libName>
  <productVersion>V100R021C00</productVersion>
  <issueDate>2021-11-01</issueDate>
</profile>`
	p := makePkg(t, fileName, map[string]string{
		"profile.xml": root,
		"resources/infocenter_service/profile.xml": profileHedex,
	})

	m, err := readMeta(p, fileName)
	if err != nil {
		t.Fatalf("readMeta 失败: %v", err)
	}
	if m.LibID != "356923456_CN" {
		t.Errorf("LibID = %q，期望取包根那份的 356923456_CN", m.LibID)
	}
	if m.LibVersion != "01" {
		t.Errorf("LibVersion = %q，期望取包根那份的 01", m.LibVersion)
	}
	if m.LibName != "SD-WAN V100R021C00 产品文档（基于iMaster NCE-Campus）" {
		t.Errorf("LibName = %q", m.LibName)
	}
}

// 包根那份是空的（或者不是我们要的那份）时要落到 HedEx 那份，
// 不能被它挡住。
func TestReadMetaFallsBackWhenRootProfileEmpty(t *testing.T) {
	const fileName = "S12700, S12700E V200R019C10 产品文档.hwics"
	p := makePkg(t, fileName, map[string]string{
		"profile.xml": `<?xml version="1.0"?><profile></profile>`,
		"resources/infocenter_service/profile.xml": profileHedex,
	})

	m, err := readMeta(p, fileName)
	if err != nil {
		t.Fatalf("readMeta 失败: %v", err)
	}
	if m.ProductVersion != "V800R021C00" {
		t.Errorf("ProductVersion = %q，期望落到 HedEx 那份", m.ProductVersion)
	}
}

// 包里一份 profile.xml 都没有时，退回文件名，且不报错。
func TestReadMetaWithoutProfile(t *testing.T) {
	const fileName = "没有元数据的包.hwics"
	p := makePkg(t, fileName, map[string]string{"resources/FileList.xml": "<fileList/>"})

	m, err := readMeta(p, fileName)
	if err != nil {
		t.Fatalf("readMeta 失败: %v", err)
	}
	if m.LibName != "没有元数据的包" {
		t.Errorf("LibName = %q，期望退回文件名", m.LibName)
	}
	if m.LibID != "没有元数据的包" {
		t.Errorf("LibID = %q，期望由文件名推导", m.LibID)
	}
	if m.ProductVersion != "" || m.IssueDate != "" {
		t.Errorf("没有 profile.xml 时不该有版本信息: %+v", m)
	}
}

// Scan 只认 .hdx / .hwics，目录里的其它文件要跳过。
func TestScanOnlyPicksPackages(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.hdx", "b.hwics", "c.txt", "d.HDX"} {
		body := map[string]string{"profile.xml": profileCamel}
		p := makePkg(t, n, body)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	// a.hdx、b.hwics、d.HDX 三个，c.txt 跳过（扩展名大小写不敏感）
	if len(idx.libs) != 3 {
		t.Fatalf("扫到 %d 个包，期望 3 个: %+v", len(idx.libs), idx.libs)
	}
}

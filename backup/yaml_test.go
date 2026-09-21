package backup

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// yamlToManifest 走一遍导入侧真正会走的路径：
// YAML → 通用值 → JSON → 结构体。
//
// 刻意不直接 Unmarshal 到结构体：导入实现就是这么做的（共用一套 struct tag，
// 避免"JSON 能读、YAML 读不出"的偏差），测试必须覆盖同一条路径。
func yamlToManifest(t *testing.T, data []byte) *Manifest {
	t.Helper()
	v, err := parseYAMLSubset(data)
	if err != nil {
		t.Fatalf("parseYAMLSubset: %v", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("重新编码成 JSON: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal 进 Manifest: %v\nJSON: %s", err, b)
	}
	return &m
}

func sampleHeader() Header {
	ttl := int64(604800)
	return Header{
		Format:        FormatTag,
		FormatVersion: FormatVersion,
		AppVersion:    "0.3.1",
		ExportedAt:    time.Date(2026, 9, 15, 11, 41, 58, 0, time.FixedZone("CST", 8*3600)),
		Platform:      "macos",
		Scope:         "full",
		Stats:         Stats{Items: 2, Categories: 1, Tags: 1, BlobBytes: 2411520},
		Settings: map[string]any{
			"capture": map[string]any{
				"enabled": true,
				"types":   []any{"text", "image", "files"},
				// 会被 yamlNeedsQuote 抓住的几种：看起来像数字、像 bool、含冒号。
				"textMaxChars": float64(262144),
				"note":         "30",
				"looksBool":    "true",
				"withColon":    "a:b",
				"withHash":     "#E24B4A",
				"empty":        "",
			},
		},
		Categories: []Category{{
			ID: 1, Name: "工作", Color: "#E24B4A", Icon: "briefcase",
			SortOrder: 0, TTLSeconds: &ttl,
			Rule: json.RawMessage(`{"match":"any","conditions":[{"field":"sourceAppId","op":"startsWith","value":"com.apple.Safari"}]}`),
		}},
		Tags: []Tag{{ID: 1, Name: "常用", Color: "#185FA5"}},
	}
}

func sampleItems() []Item {
	text := "中文文档 链接 图片\n第二行带换行"
	url := "https://example.com/page?a=1"
	lastUsed := time.Date(2026, 9, 15, 11, 30, 11, 0, time.FixedZone("CST", 8*3600))
	expires := time.Date(2026, 10, 15, 11, 30, 11, 0, time.FixedZone("CST", 8*3600))
	rtfPath := "blobs/3b/07/" + strings.Repeat("3b", 32) + ".rtf"
	return []Item{
		{
			ID: 1024, Kind: "text", Preview: "中文文档 链接",
			Fingerprint: "sha256:" + strings.Repeat("ab", 32), ByteSize: 42,
			Text: &text, FilePaths: nil,
			SourceAppID: "com.apple.Safari", SourceAppName: "Safari", SourceURL: &url,
			CategoryID: ptrI64(1), TagIDs: []int64{1, 2},
			Pinned:     false,
			TTLSeconds: ptrI64(2592000), ExpiresAt: &expires,
			FirstSeenAt: time.Date(2026, 9, 15, 9, 12, 3, 0, time.FixedZone("CST", 8*3600)),
			CreatedAt:   lastUsed, LastUsedAt: &lastUsed, UseCount: 3,
			Blobs: nil,
		},
		{
			ID: 1025, Kind: "mixed", Preview: "图片 1080 × 1920",
			Fingerprint: "sha256:" + strings.Repeat("cd", 32), ByteSize: 2411520,
			// pinned 时 ExpiresAt 必须为 nil（§3.5）。
			Pinned:      true,
			ImageWidth:  1080,
			ImageHeight: 1920,
			FirstSeenAt: lastUsed, CreatedAt: lastUsed, UseCount: 1,
			Blobs: []BlobRef{
				{Role: "image", Path: "blobs/cd/cd/" + strings.Repeat("cd", 32) + ".png",
					Mime: "image/png", Bytes: 2411520, SHA256: strings.Repeat("cd", 32)},
				{Role: "rtf", Path: rtfPath, Mime: "application/rtf", Bytes: 128, SHA256: strings.Repeat("3b", 32)},
			},
		},
	}
}

func ptrI64(n int64) *int64 { return &n }

// jsonManifest 用 JSON 走一遍，作为 YAML 的对照基准。
func jsonManifest(t *testing.T, h Header, items []Item) *Manifest {
	return jsonManifestFull(t, h, items, nil)
}

// jsonManifestFull 是带上草稿段的版本（草稿与 items 两条路径都要能对照）。
func jsonManifestFull(t *testing.T, h Header, items []Item, drafts []Draft) *Manifest {
	t.Helper()
	b, err := json.Marshal(Manifest{Header: h, Items: items, Drafts: drafts})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &m
}

// writeYAMLManifest 用**产品代码里那个发射器**生成一份 YAML 清单。
//
// 第一版测试是手工调 writeYAMLHeader + writeYAMLItem 拼的，于是
// "段的键名怎么写、空集合怎么写"这一层**完全没被覆盖**——而
// `items:` 后面跟一个缩进的 `[]` 恰恰是非法 YAML（独立一行的 `[]`
// 不是合法的节点续行）。测试必须走产品代码走的那条路，否则测的是
// 一份没人会生成的文本。
func writeYAMLManifest(t *testing.T, h Header, items []Item, drafts []Draft) []byte {
	t.Helper()
	var buf bytes.Buffer
	out := &yamlManifestWriter{w: &buf}
	if err := out.WriteHeader(h); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := out.BeginItems(); err != nil {
		t.Fatalf("BeginItems: %v", err)
	}
	for i := range items {
		if err := out.WriteItem(items[i]); err != nil {
			t.Fatalf("WriteItem[%d]: %v", i, err)
		}
	}
	if err := out.EndItems(); err != nil {
		t.Fatalf("EndItems: %v", err)
	}
	if err := out.BeginDrafts(); err != nil {
		t.Fatalf("BeginDrafts: %v", err)
	}
	for i := range drafts {
		if err := out.WriteDraft(drafts[i]); err != nil {
			t.Fatalf("WriteDraft[%d]: %v", i, err)
		}
	}
	if err := out.EndDrafts(); err != nil {
		t.Fatalf("EndDrafts: %v", err)
	}
	return buf.Bytes()
}

// YAML 与 JSON 两条路径必须产出**完全一致**的清单。
//
// 这是整个备份格式的核心不变量：如果两者不一致，那么"用 YAML 备份、
// 用 JSON 的规则导入"会静默丢字段。用 reflect.DeepEqual 逐字段比，
// 比逐字段手写断言更能抓住"漏了一个字段"。
func TestYAML_RoundTripMatchesJSON(t *testing.T) {
	h := sampleHeader()
	items := sampleItems()

	raw := writeYAMLManifest(t, h, items, nil)

	got := yamlToManifest(t, raw)
	want := jsonManifest(t, h, items)

	if len(got.Items) != len(want.Items) {
		t.Fatalf("条目数 %d，想要 %d\nYAML:\n%s", len(got.Items), len(want.Items), raw)
	}
	if !reflect.DeepEqual(normalizeCategories(got.Categories), normalizeCategories(want.Categories)) {
		t.Errorf("categories 不一致\n got %+v\nwant %+v", got.Categories, want.Categories)
	}
	if !reflect.DeepEqual(got.Tags, want.Tags) {
		t.Errorf("tags 不一致\n got %+v\nwant %+v", got.Tags, want.Tags)
	}
	if !reflect.DeepEqual(got.Stats, want.Stats) {
		t.Errorf("stats 不一致\n got %+v\nwant %+v", got.Stats, want.Stats)
	}
	canonItems(got.Items)
	canonItems(want.Items)
	for i := range want.Items {
		if !reflect.DeepEqual(got.Items[i], want.Items[i]) {
			g, _ := json.MarshalIndent(got.Items[i], "", "  ")
			w, _ := json.MarshalIndent(want.Items[i], "", "  ")
			t.Errorf("items[%d] 不一致\nYAML→ %s\nJSON→ %s", i, g, w)
		}
	}
}

// 草稿段也要能无损往返：正文里的换行、缩进、`#`、引号、以及
// `](blobs/…)` 形式的图片引用全都不能在路上被 YAML 的规则吃掉。
//
// 这一条最容易被"多引几个引号"的偷懒实现蒙过去：单引号里换行会被
// **折叠成空格**，正文就那么静默地变了样——而草稿的正文就是全部内容，
// 变样等于丢数据。
func TestYAML_DraftsRoundTrip(t *testing.T) {
	h := sampleHeader()
	h.Stats.Drafts = 2
	created := time.Date(2026, 9, 15, 11, 30, 11, 0, time.FixedZone("CST", 8*3600))
	drafts := []Draft{
		{
			ID:        1,
			Title:     "草稿 1",
			MD:        "第一行\n第二行\n\n- 列表项\n- 另一个\n\n# 标题\n\n![图](blobs/9f/2a/9f2a.png)\n\n[链接](https://example.com/a?b=1&c=2)\n",
			CreatedAt: created,
			UpdatedAt: created.Add(90 * time.Second),
		},
		{
			ID:    2,
			Title: "", // 用户清空过标题，空标题是合法状态
			// 会被 YAML 误读的字符：冒号、井号、引号、行首短横、纯数字。
			MD:        "key: value\n# 这不是注释\n他说：\"行\"\n- 3\n\n2026\n",
			CreatedAt: created,
			UpdatedAt: created,
		},
	}

	raw := writeYAMLManifest(t, h, nil, drafts)
	got := yamlToManifest(t, raw)
	want := jsonManifestFull(t, h, nil, drafts)

	if len(got.Drafts) != len(want.Drafts) {
		t.Fatalf("草稿数 %d，想要 %d\nYAML:\n%s", len(got.Drafts), len(want.Drafts), raw)
	}
	if !reflect.DeepEqual(got.Stats, want.Stats) {
		t.Errorf("stats 不一致\n got %+v\nwant %+v", got.Stats, want.Stats)
	}
	for i := range want.Drafts {
		if !reflect.DeepEqual(got.Drafts[i], want.Drafts[i]) {
			t.Errorf("drafts[%d] 不一致\nYAML→ %+v\nJSON→ %+v", i, got.Drafts[i], want.Drafts[i])
		}
	}
	// 反向也钉一下：正文必须逐字节相同，不能只是"DeepEqual 通过"。
	// （DeepEqual 比的是 Go 字符串，所以这一条是冗余的——留着是因为
	// 它把"换行被折叠"这件事的**后果**写在了断言里，读的人一眼能懂。）
	if got.Drafts[0].MD != drafts[0].MD {
		t.Errorf("草稿正文被改动了\n got %q\nwant %q", got.Drafts[0].MD, drafts[0].MD)
	}
}

// 空集合的回归：`items:` 与 `drafts:` 在一条都没有时必须写成
// **同一行**的 `items: []`。
//
// 写成两行（`items:` 换行再缩进一个 `[]`）产出的 YAML 我们自己的解析器
// 都读不回来——旧代码正是这么写的，而旧测试因为手工拼 items 而
// 完全没走到这条路径。这个用例是补上那个洞的。
func TestYAML_EmptyCollectionsAreSingleLine(t *testing.T) {
	h := sampleHeader()
	raw := writeYAMLManifest(t, h, nil, nil)
	s := string(raw)

	if !strings.Contains(s, "\nitems: []\n") {
		t.Errorf("空 items 应写成同一行的 `items: []`\n--- YAML ---\n%s", s)
	}
	if !strings.Contains(s, "\ndrafts: []\n") {
		t.Errorf("空 drafts 应写成同一行的 `drafts: []`\n--- YAML ---\n%s", s)
	}
	got := yamlToManifest(t, raw)
	if len(got.Items) != 0 || len(got.Drafts) != 0 {
		t.Fatalf("空集合读回来应为空：items=%d drafts=%d", len(got.Items), len(got.Drafts))
	}
}

// 时间字段必须是带时区偏移的 ISO-8601 —— 不能是裸 epoch（§3.5）。
// 这条同时钉住"跨时区迁移不能出错"这个需求：偏移写进去了，
// 导入方转 epoch 时用的就是同一个**时刻**。
func TestYAML_TimeFieldsCarryOffset(t *testing.T) {
	h := sampleHeader()
	var buf bytes.Buffer
	if err := writeYAMLHeader(&buf, h); err != nil {
		t.Fatalf("writeYAMLHeader: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "+08:00") {
		t.Fatalf("导出的时间没带时区偏移：\n%s", out)
	}
	if strings.Contains(out, "exportedAt: 17") {
		t.Fatal("导出成了裸 epoch")
	}

	got := yamlToManifest(t, buf.Bytes())
	if !got.ExportedAt.Equal(h.ExportedAt) {
		t.Fatalf("导出时间往返变了：%v → %v", h.ExportedAt, got.ExportedAt)
	}
	// 时刻必须一致，不管落在哪个时区里展示。
	if got.ExportedAt.UTC() != h.ExportedAt.UTC() {
		t.Fatal("往返后时刻发生了变化")
	}
}

// 那些"不引号就会被误读"的字符串必须原样往返。
func TestYAML_ScalarQuotingPreservesValues(t *testing.T) {
	cases := []string{
		"#E24B4A",      // 会被当成注释
		"a:b",          // 会被当成嵌套键
		"30",           // 会被当成数字
		"262144",       // 同上
		"true",         // 会被当成布尔
		"false",        // 同上
		"null",         // 会被当成空
		"~",            // 同上
		"yes",          // 某些解析器会当成 true
		"",             // 空串
		" leading",     // 前导空格
		"trailing ",    // 尾随空格
		"- dash",       // 行首短横
		"a, b",         // 会被当成流式序列
		"[1, 2]",       // 会被当成序列
		"{}",           // 会被当成 map
		"it's",         // 单引号需要转义成 ''
		"line1\nline2", // 换行必须转义，不能被折叠成空格
		"tab\there",
		"emoji 🐾 中文",
		"back\\slash",
		`quote"inside`,
	}
	for _, want := range cases {
		t.Run(strings.ReplaceAll(want, "\n", "\\n"), func(t *testing.T) {
			var buf bytes.Buffer
			yamlIndent(&buf, 0)
			buf.WriteString("v: " + yamlEmit(want) + "\n")
			v, err := parseYAMLSubset(buf.Bytes())
			if err != nil {
				t.Fatalf("解析 %q 时出错：%v（YAML: %q）", want, err, buf.String())
			}
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("顶层不是 mapping")
			}
			got, ok := m["v"].(string)
			if !ok {
				t.Fatalf("%q 解析出来不是字符串，而是 %T（%v）——YAML: %q", want, m["v"], m["v"], buf.String())
			}
			if got != want {
				t.Fatalf("往返不一致：%q → %q（YAML: %q）", want, got, buf.String())
			}
		})
	}
}

func TestYAML_NullAndEmptyCollections(t *testing.T) {
	h := sampleHeader()
	// 空集合要能被读回"空"而不是 nil 指针崩掉。
	it := Item{
		ID: 1, Kind: "text", Preview: "x",
		Fingerprint: "sha256:" + strings.Repeat("11", 32),
		FirstSeenAt: time.Now(), CreatedAt: time.Now(),
	}
	var buf bytes.Buffer
	if err := writeYAMLHeader(&buf, h); err != nil {
		t.Fatalf("writeYAMLHeader: %v", err)
	}
	// 键名由发射器惰性写出（见 writer.go）：这里手工补上，因为本用例要
	// 单独测 writeYAMLItem 的字段形态，不经过 yamlManifestWriter。
	buf.WriteString("items:\n")
	if err := writeYAMLItem(&buf, it, 1); err != nil {
		t.Fatalf("writeYAMLItem: %v", err)
	}
	got := yamlToManifest(t, buf.Bytes())
	if len(got.Items) != 1 {
		t.Fatalf("条目数 = %d，想要 1", len(got.Items))
	}
	g := got.Items[0]
	for _, c := range []struct {
		name  string
		isNil bool
	}{
		{"text", g.Text == nil},
		{"html", g.HTML == nil},
		{"rtfPath", g.RTFPath == nil},
		{"sourceUrl", g.SourceURL == nil},
		{"categoryId", g.CategoryID == nil},
		{"ttlSeconds", g.TTLSeconds == nil},
		{"expiresAt", g.ExpiresAt == nil},
		{"lastUsedAt", g.LastUsedAt == nil},
	} {
		if !c.isNil {
			t.Errorf("%s 应为 null", c.name)
		}
	}
	if len(g.FilePaths) != 0 || len(g.TagIDs) != 0 || len(g.Blobs) != 0 {
		t.Errorf("空数组应读回空：filePaths=%v tagIds=%v blobs=%v", g.FilePaths, g.TagIDs, g.Blobs)
	}
}

// 坏输入必须**明确失败**，不能猜。
func TestYAML_RejectsUnsupportedInput(t *testing.T) {
	cases := map[string]string{
		"制表符缩进":      "a:\n\tb: 1\n",
		"多文档标记":      "---\na: 1\n",
		"锚点":         "a: &x 1\n",
		"折叠块":        "a: >\n  text\n",
		"行首非键值":      "just a bare line\n",
		"双引号未闭合":     "a: \"unterminated\n",
		"单引号未闭合":     "a: 'unterminated\n",
		"流式序列未闭合":    "a: [1, 2\n",
		"流式 mapping": "a: {b: 1}\n",
		"空文档":        "\n\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseYAMLSubset([]byte(in)); err == nil {
				t.Fatalf("应报错但通过了：%q", in)
			}
		})
	}
}

// 注释与空行应当被忽略，且引号内的 `#` 不能被当注释。
func TestYAML_CommentsAndBlankLines(t *testing.T) {
	in := "" +
		"# 顶部注释\n" +
		"format: pawclip.backup\n" +
		"\n" +
		"name: '#E24B4A'   # 行尾注释\n" +
		"note: 'a # b'\n" +
		"list:\n" +
		"  - 1\n" +
		"  - 2\n"
	v, err := parseYAMLSubset([]byte(in))
	if err != nil {
		t.Fatalf("parseYAMLSubset: %v", err)
	}
	m := v.(map[string]any)
	if m["format"] != "pawclip.backup" {
		t.Errorf("format = %v", m["format"])
	}
	if m["name"] != "#E24B4A" {
		t.Errorf("name = %v（引号内的 # 被当注释了？）", m["name"])
	}
	if m["note"] != "a # b" {
		t.Errorf("note = %v", m["note"])
	}
	list, ok := m["list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("list = %#v", m["list"])
	}
}

// 深嵌套的 settings 也要能往返（§3.1 的 settings 是任意结构）。
func TestYAML_DeeplyNestedSettings(t *testing.T) {
	h := Header{
		Format: FormatTag, FormatVersion: 1, AppVersion: "0.3.1",
		ExportedAt: time.Now(), Platform: "macos", Scope: "full",
		Settings: map[string]any{
			"a": map[string]any{
				"b": map[string]any{
					"c": []any{"x", "y"},
					"d": float64(3),
				},
				"e": true,
			},
		},
	}
	var buf bytes.Buffer
	if err := writeYAMLHeader(&buf, h); err != nil {
		t.Fatalf("writeYAMLHeader: %v", err)
	}
	got := yamlToManifest(t, buf.Bytes())
	if !reflect.DeepEqual(got.Settings, h.Settings) {
		t.Fatalf("settings 往返不一致：\n got %#v\nwant %#v\nYAML:\n%s",
			got.Settings, h.Settings, buf.String())
	}
}

// 发射器的 quoting 判定本身。
func TestYAML_NeedsQuoteTable(t *testing.T) {
	need := []string{"#c", "a:b", "30", "true", "null", "", " x", "x ", "a, b", "[1]", "{a}", "a|b", "a*b", "~", "1.5", "-x", "a%b", "sha256:abc"}
	// 注意 `sha256:...` 归在"需要引号"一侧：虽然裸写也能解析，
	// 但保守加引号更稳妥，且 §3.2 的示例本身就带引号。
	free := []string{"pawclip.backup", "工作", "full", "macos", "com.apple.Safari", "hello-world", "a_b"}
	for _, s := range need {
		if !yamlNeedsQuote(s) {
			t.Errorf("%q 应当需要引号", s)
		}
	}
	for _, s := range free {
		if yamlNeedsQuote(s) {
			t.Errorf("%q 不该需要引号", s)
		}
	}
}

// mime → 压缩策略（§6）。
func TestManifest_CompressionPolicy(t *testing.T) {
	store := []string{"image/png", "image/jpeg", "image/webp", "image/gif"}
	deflate := []string{"image/bmp", "image/tiff", "application/rtf", "text/html", "text/plain", "application/json"}
	for _, m := range store {
		if !UseStore(m) {
			t.Errorf("%s 应当用 store（已压缩格式，再 deflate 是浪费）", m)
		}
	}
	for _, m := range deflate {
		if UseStore(m) {
			t.Errorf("%s 应当用 deflate", m)
		}
	}
	if ExtForMime("image/png") != "png" || ExtForMime("application/rtf") != "rtf" {
		t.Error("ExtForMime 映射不对")
	}
	if MimeForExt("rtf") != "application/rtf" || MimeForExt(".PNG") != "image/png" {
		t.Error("MimeForExt 映射不对")
	}
}

func TestManifest_Validate(t *testing.T) {
	ok := Item{Kind: "text", Fingerprint: "sha256:" + strings.Repeat("ab", 32)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法条目报错：%v", err)
	}

	bad := map[string]Item{
		"fingerprint 没有前缀": {Kind: "text", Fingerprint: strings.Repeat("ab", 32)},
		"fingerprint 太短":   {Kind: "text", Fingerprint: "sha256:abcd"},
		"fingerprint 大写":   {Kind: "text", Fingerprint: "sha256:" + strings.Repeat("AB", 32)},
		"kind 为空":          {Fingerprint: "sha256:" + strings.Repeat("ab", 32)},
		"pinned 却有过期时间":    {Kind: "text", Fingerprint: "sha256:" + strings.Repeat("ab", 32), Pinned: true, ExpiresAt: &[]time.Time{time.Now()}[0]},
		"blob sha 非法":      {Kind: "text", Fingerprint: "sha256:" + strings.Repeat("ab", 32), Blobs: []BlobRef{{Role: "image", SHA256: "zz"}}},
		"blob 缺 role":      {Kind: "text", Fingerprint: "sha256:" + strings.Repeat("ab", 32), Blobs: []BlobRef{{SHA256: strings.Repeat("ab", 32)}}},
	}
	for name, it := range bad {
		if err := it.Validate(); err == nil {
			t.Errorf("%s：应当报错", name)
		}
	}
}

// normalizeCategories 把 rule 从原始 JSON 字节规范化成通用值再比较。
//
// 为什么需要：JSON 路径保留原始字节顺序，而 YAML 路径必然经过
// map[string]any（键会被排序）。两者**语义相同、字节不同**，
// 直接比 RawMessage 会误报。这里比的是语义。
func normalizeCategories(in []Category) []Category {
	out := make([]Category, len(in))
	copy(out, in)
	for i := range out {
		if len(out[i].Rule) == 0 {
			continue
		}
		var v any
		if err := json.Unmarshal(out[i].Rule, &v); err != nil {
			continue
		}
		nb, err := json.Marshal(v)
		if err == nil {
			out[i].Rule = nb
		}
	}
	return out
}

// canonItems 把"空切片"与"nil 切片"统一成同一种形态。
//
// JSON 的 nil 切片编成 null，YAML 里我们写成 `[]`（更易读），两者
// 语义完全一样。不让这层表示差异把测试搞红——真正要盯的是
// "字段有没有丢、值有没有变"。
func canonItems(items []Item) {
	for i := range items {
		items[i].FilePaths = canonSlice(items[i].FilePaths)
		items[i].TagIDs = canonSlice(items[i].TagIDs)
		items[i].Blobs = canonSlice(items[i].Blobs)
	}
}

func canonSlice[T any](in []T) []T {
	if len(in) == 0 {
		return nil
	}
	return in
}

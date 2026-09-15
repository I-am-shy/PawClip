package clipboard

import (
	"strings"
	"sync/atomic"
	"time"
)

// 本文件实现 DESIGN.md §14 第 17 条规定的过滤顺序：
//
//	保密标记 → 应用黑名单 → 类型开关 → 自写入守卫 → 尺寸上限
//
// 把最便宜的检查放最前面，避免对大图片做无用功（例如 sha256 10 MB 的图）。
// 唯一的例外是"内容为空"的前置判定——它不是策略决策，而是后续所有检查的前提。

// Decision 是一次过滤的结论。
type Decision int

const (
	// Accept 表示放行，进入归一化与落库。
	Accept Decision = iota
	// DropCaptureDisabled 捕获总开关关闭。
	DropCaptureDisabled
	// DropPrivate 内容带平台原生"请勿记录"标记。
	DropPrivate
	// DropAppExcluded 来源应用在黑名单里。
	DropAppExcluded
	// DropTypeDisabled 内容的表示组不在 capture.types 里。
	DropTypeDisabled
	// DropSelfWrite 是我们自己刚写回剪贴板的内容。
	DropSelfWrite
	// DropDebounced 同一指纹在 debounceMs 内重复到达。
	DropDebounced
	// DropTooLarge 超过 capture.imageMaxBytes。
	DropTooLarge
	// DropEmpty 快照里没有任何我们认识的表示（是前置判定，不是策略）。
	DropEmpty
)

// 决策名。写成字符串是为了落日志与测试断言可读，也方便将来直接进统计面板。
var decisionNames = map[Decision]string{
	Accept:              "accept",
	DropCaptureDisabled: "drop:capture_disabled",
	DropPrivate:         "drop:private_type",
	DropAppExcluded:     "drop:app_excluded",
	DropTypeDisabled:    "drop:type_disabled",
	DropSelfWrite:       "drop:self_write",
	DropDebounced:       "drop:debounced",
	DropTooLarge:        "drop:too_large",
	DropEmpty:           "drop:empty",
}

func (d Decision) String() string {
	if s, ok := decisionNames[d]; ok {
		return s
	}
	return "drop:unknown"
}

// Dropped 判断是否被丢弃。
func (d Decision) Dropped() bool { return d != Accept }

// FilterConfig 是 filter 的输入，取自 settings 表（DESIGN.md §9）。
type FilterConfig struct {
	// Enabled ← capture.enabled，捕获总开关
	Enabled bool
	// Types ← capture.types，捕获的表示组（text / image / files）
	Types []string
	// ExcludeApps ← exclude.apps，应用黑名单，支持 * 与 ? 通配
	ExcludeApps []string
	// ExcludePrivateTypes ← exclude.privateTypes
	ExcludePrivateTypes bool
	// ImageMaxBytes ← capture.imageMaxBytes，超限图片不记录
	ImageMaxBytes int64
	// DebounceMs ← capture.debounceMs，连续变更合并窗口
	DebounceMs int
}

// DefaultFilterConfig 是 §9 里与 filter 相关的默认值。
func DefaultFilterConfig() FilterConfig {
	return FilterConfig{
		Enabled:             true,
		Types:               []string{KindText, KindImage, KindFiles},
		ExcludeApps:         []string{"com.1password.*", "com.apple.keychainaccess", "com.agilebits.*"},
		ExcludePrivateTypes: true,
		ImageMaxBytes:       10 * 1024 * 1024,
		DebounceMs:          120,
	}
}

// filterState 是过滤器的**可替换配置**。
//
// 为什么把配置装进一个不可变结构体、用 atomic.Pointer 换掉整只，
// 而不是给每个字段加锁：
//
//   - ①`Decide` 在**轮询热路径**上（macOS 每 0.2s 一次，DESIGN §14 第 9 条
//     要求"绝不分配"）。atomic.Pointer.Load 是一条无锁读，不加锁、不分配。
//   - ② 热重载是低频动作。整体替换的语义也比逐字段改更清楚：
//     "某一刻起，判定规则换成了另一套"，不会出现"types 已经是新的、
//     apps 还是旧的"这种半更新状态。
type filterState struct {
	types map[string]bool
	apps  []string // 已统一小写的通配模式
	cfg   FilterConfig
}

// Filter 把"这条剪贴板快照该不该记录"收敛成一个纯函数式判定。
type Filter struct {
	state *atomic.Pointer[filterState]
	// debouncer 是**有状态**的，所以不能跟着 state 一起换
	// （换掉会让"刚去抖过的内容"重新变成可记录）。重配时只 Reset 它。
	debouncer *Debouncer
	guard     *SelfWriteGuard

	// 计数，仅供诊断与验收取证。
	counts [10]int64
}

// newFilterState 把配置编译成可用的判定状态。
func newFilterState(cfg FilterConfig) *filterState {
	st := &filterState{
		types: make(map[string]bool, len(cfg.Types)),
		cfg:   cfg,
	}
	for _, t := range cfg.Types {
		st.types[strings.ToLower(strings.TrimSpace(t))] = true
	}
	for _, a := range cfg.ExcludeApps {
		a = strings.TrimSpace(a)
		if a != "" {
			st.apps = append(st.apps, strings.ToLower(a))
		}
	}
	return st
}

// NewFilter 构造过滤器。guard 为 nil 时跳过自写入守卫。
func NewFilter(cfg FilterConfig, guard *SelfWriteGuard) *Filter {
	f := &Filter{
		state:     &atomic.Pointer[filterState]{},
		guard:     guard,
		debouncer: NewDebouncer(time.Duration(cfg.DebounceMs) * time.Millisecond),
	}
	f.state.Store(newFilterState(cfg))
	return f
}

// Apply 热替换判定配置（设置界面改完立刻生效）。
//
// 去抖器会被 Reset：换了规则之后，"这个指纹刚刚去过抖"这个记忆不该继续
// 约束新规则。反过来保留它会造成一种很难查的现象——用户改了配置、
// 又立刻复制同一份内容，结果什么都不发生（因为旧规则刚把它去抖掉了）。
//
// 计数器**不重置**：它们是累计证据。重置会让验收里"累计丢弃 = N"这类
// 断言在改一次设置后莫名其妙地倒退。
func (f *Filter) Apply(cfg FilterConfig) {
	f.state.Store(newFilterState(cfg))
	f.debouncer.Reset()
}

// Config 返回当前生效的配置副本（诊断用）。
func (f *Filter) Config() FilterConfig {
	if st := f.state.Load(); st != nil {
		return st.cfg
	}
	return FilterConfig{}
}

// Decide 给出结论。fp 是调用方预先算好的内容指纹（*Raw).Fingerprint()）。
// now 由调用方传入，便于测试。
func (f *Filter) Decide(r *Raw, fp string, now time.Time) Decision {
	d := f.decide(r, fp, now)
	atomic.AddInt64(&f.counts[d], 1)
	return d
}

func (f *Filter) decide(r *Raw, fp string, now time.Time) Decision {
	// 前置：完全空的快照没有任何后续判断的意义
	if r == nil {
		return DropEmpty
	}
	c := r.Content()
	if c.Empty() {
		return DropEmpty
	}

	// 热路径上只读一次原子指针。之后所有判定都用这一份快照——
	// 读到一半被别人换掉的话，就会出现"用新规则判类型、用旧规则判尺寸"
	// 这种自相矛盾的结论。
	st := f.state.Load()
	if st == nil {
		return DropEmpty
	}

	// ① 总开关
	if !st.cfg.Enabled {
		return DropCaptureDisabled
	}

	// ② 保密标记：纯类型名查表，零分配
	if st.cfg.ExcludePrivateTypes && IsPrivateTypeNames(r.RawTypes) {
		return DropPrivate
	}

	// ③ 应用黑名单：只在配置非空时才做字符串匹配
	if len(st.apps) > 0 && appExcludedIn(st.apps, r.SourceAppID, r.SourceAppName) {
		return DropAppExcluded
	}

	// ④ 类型开关
	if !anyTypeEnabledIn(st.types, c) {
		return DropTypeDisabled
	}

	// ⑤ 自写入守卫：指纹比对
	if f.guard != nil && f.guard.ShouldDrop(fp) {
		return DropSelfWrite
	}

	// ⑥ 去抖：同指纹在窗口内重复
	if !f.debouncer.ShouldProcess(fp, now) {
		return DropDebounced
	}

	// ⑦ 尺寸上限
	if r.Image != nil && st.cfg.ImageMaxBytes > 0 && int64(len(r.Image.PNG)) > st.cfg.ImageMaxBytes {
		return DropTooLarge
	}

	return Accept
}

// anyTypeEnabledIn 判断内容里有没有任何一组被 capture.types 允许。
//
// 判定用"表示组"而不是 items.kind：浏览器复制一份内容会同时带
// text + html 两个 flavor，若按 kind 判定，"html" 不在默认列表里就会
// 把绝大多数网页复制全部丢掉。
func anyTypeEnabledIn(types map[string]bool, c Content) bool {
	groups := c.Groups()
	if len(groups) == 0 {
		return false
	}
	for _, g := range groups {
		if types[g] {
			return true
		}
	}
	return false
}

// appExcludedIn 同时拿 bundle id / exe 路径与显示名去比。
func appExcludedIn(apps []string, id, name string) bool {
	candidates := make([]string, 0, 3)
	if id != "" {
		candidates = append(candidates, id)
	}
	if name != "" {
		candidates = append(candidates, name)
	}
	for _, cand := range candidates {
		lower := strings.ToLower(cand)
		for _, pat := range apps {
			if globMatch(pat, lower) {
				return true
			}
		}
	}
	return false
}

// ResetDebounce 在暂停后重新开始时清掉去抖状态。
func (f *Filter) ResetDebounce() { f.debouncer.Reset() }

// Stats 返回各结论的累计次数（索引即 Decision 值）。验收取证用。
func (f *Filter) Stats() map[string]int64 {
	out := make(map[string]int64, len(f.counts))
	for i := range f.counts {
		if n := atomic.LoadInt64(&f.counts[i]); n > 0 {
			out[Decision(i).String()] = n
		}
	}
	return out
}

// globMatch 做大小写不敏感的通配匹配：* 匹配任意长度（可跨路径分隔符），
// ? 匹配单字符。
//
// 不用 path.Match 的原因：它把 '/' 当分隔符，'*' 不跨分隔符——而我们要匹配的
// 既有 bundle id（com.1password.*）也有 Windows 的 exe 全路径
// （C:\Program Files\...\1Password.exe），后者用 path.Match 会漏。
func globMatch(pattern, s string) bool {
	// 经典的双指针 + 回溯实现，最坏 O(n*m)，无额外分配
	var (
		p, si        int
		starP, starS = -1, 0
	)
	for si < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == s[si] || pattern[p] == '?'):
			p++
			si++
		case p < len(pattern) && pattern[p] == '*':
			starP = p
			starS = si
			p++
		case starP >= 0:
			p = starP + 1
			starS++
			si = starS
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

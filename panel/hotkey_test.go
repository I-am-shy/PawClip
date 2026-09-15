package panel

import (
	"strings"
	"testing"
)

// 热键解析是本包唯一"纯逻辑"的部分，也是唯一能在 mac 上被单测直接钉住的
// 部分（键码映射是平台相关的，见 darwin.go / windows.go）。
//
// 它值得认真测的原因：解析错了不会报错，只会**注册出一个错的全局热键**。
// 那是最难查的一类问题——用户按 ⌘⇧V 没反应，而程序认为自己注册成功了。

func TestParseHotkey_OK(t *testing.T) {
	cases := []struct {
		in   string
		want Hotkey
	}{
		// DESIGN §9 的默认值。CmdOrCtrl 必须落成独立的字段，
		// 而不是"Cmd 与 Ctrl 都置上"——否则 macOS 上会注册出 ⌘+Ctrl+Shift+V。
		{"CmdOrCtrl+Shift+V", Hotkey{Key: "V", CmdOrCtrl: true, Shift: true}},
		{"CommandOrControl+Shift+V", Hotkey{Key: "V", CmdOrCtrl: true, Shift: true}},
		{"cmdorctrl+shift+v", Hotkey{Key: "V", CmdOrCtrl: true, Shift: true}},
		{"Cmd+Space", Hotkey{Key: "SPACE", Cmd: true}},
		{"Meta+Space", Hotkey{Key: "SPACE", Cmd: true}},
		{"Super+1", Hotkey{Key: "1", Cmd: true}},
		{"Ctrl+Alt+P", Hotkey{Key: "P", Ctrl: true, Alt: true}},
		{"Option+F12", Hotkey{Key: "F12", Alt: true}},
		{"Shift+Left", Hotkey{Key: "LEFT", Shift: true}},
		{"Ctrl+Escape", Hotkey{Key: "ESC", Ctrl: true}},
		{"Ctrl+Return", Hotkey{Key: "ENTER", Ctrl: true}},
		// 键名大小写不敏感，但结果必须归一化成大写。
		{"ctrl+delete", Hotkey{Key: "DELETE", Ctrl: true}},
		{"  Ctrl + Shift + G  ", Hotkey{Key: "G", Ctrl: true, Shift: true}},
	}
	for _, c := range cases {
		got, err := ParseHotkey(c.in)
		if err != nil {
			t.Errorf("ParseHotkey(%q) 报错：%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseHotkey(%q) = %+v，想要 %+v", c.in, got, c.want)
		}
	}
}

func TestParseHotkey_Errors(t *testing.T) {
	bad := []struct {
		in     string
		reason string
	}{
		{"", "空串"},
		{"   ", "空白"},
		// 裸键的全局热键等于把那个键从整个系统吃掉，必须拒绝（§9 的警告同源）。
		{"V", "没有修饰键"},
		{"F12", "没有修饰键"},
		{"Space", "没有修饰键"},
		{"Ctrl", "只有修饰键"},
		{"Shift+Ctrl", "只有修饰键"},
		{"Ctrl+Shift+", "尾随加号"},
		{"Ctrl++V", "空分段"},
		{"Ctrl+Shift+NotAKey", "不认识的键名"},
		{"Ctrl+Shift+AB", "两字符键名"},
		{"Shift+V+Ctrl", "主键不在最后"},
	}
	for _, c := range bad {
		if _, err := ParseHotkey(c.in); err == nil {
			t.Errorf("ParseHotkey(%q) 应当报错（%s），却成功了", c.in, c.reason)
		}
	}
}

// TestParseHotkey_ErrorMessagesAreActionable 要求报错里带上原始输入。
//
// 热键是从设置界面的文本框里来的，用户改错了必须能看懂是哪一条错。
func TestParseHotkey_ErrorMessagesAreActionable(t *testing.T) {
	_, err := ParseHotkey("Ctrl+Shift+NotAKey")
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "Ctrl+Shift+NotAKey") {
		t.Errorf("报错信息里没有原始输入：%v", err)
	}
}

func TestHotkey_String(t *testing.T) {
	cases := []struct {
		in   Hotkey
		want string
	}{
		{Hotkey{Key: "V", CmdOrCtrl: true, Shift: true}, "CmdOrCtrl+Shift+V"},
		{Hotkey{Key: "P", Ctrl: true, Alt: true}, "Ctrl+Alt+P"},
		{Hotkey{Key: "F5", Cmd: true}, "Cmd+F5"},
		// 具名键用 Canonical 的大写形式：String() 输出的是**规范形式**
		// 而不是"好看的"形式，这样它可以直接回填进设置界面的输入框并被
		// 原样解析回来（见下面的往返断言）。
		{Hotkey{Key: "SPACE", Cmd: true}, "Cmd+SPACE"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("String() = %q，想要 %q", got, c.want)
		}
	}

	// 关键不变量：String() 的输出必须能被 ParseHotkey 原样解析回同一个热键。
	// 设置界面拿 String() 显示当前值、用户点保存时又把它送回来解析，
	// 这个往返一旦不闭合，用户就会遇到"打开设置什么都没改，保存后热键变了"。
	roundTrips := []string{
		"CmdOrCtrl+Shift+V", "Ctrl+Alt+P", "Cmd+SPACE", "Ctrl+ESC",
		"Shift+F12", "Cmd+ENTER", "Ctrl+Shift+Delete",
	}
	for _, s := range roundTrips {
		h, err := ParseHotkey(s)
		if err != nil {
			t.Fatalf("ParseHotkey(%q): %v", s, err)
		}
		again, err := ParseHotkey(h.String())
		if err != nil {
			t.Fatalf("ParseHotkey(String()=%q): %v", h.String(), err)
		}
		if again != h {
			t.Errorf("往返不一致：%q → %+v → %q → %+v", s, h, h.String(), again)
		}
	}
}

func TestAction_Valid(t *testing.T) {
	// 跨语言边界传过来的是裸整数，越界必须被挡掉。
	if Action(-1).Valid() {
		t.Error("-1 应当是非法动作")
	}
	if Action(actionMax).Valid() {
		t.Error("上界本身应当是非法动作")
	}
	if !ActionShow.Valid() || !ActionQuit.Valid() {
		t.Error("已定义的动作应当是合法的")
	}
	// 每个定义的动作都要有可读名字，否则日志里会出现裸数字。
	for a := Action(0); a < actionMax; a++ {
		if strings.HasPrefix(a.String(), "action(") {
			t.Errorf("动作 %d 没有名字", a)
		}
	}
}

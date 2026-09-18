//go:build !darwin

package panel

// RunOpenDialog 的非 darwin 占位。
//
// macOS 上文件对话框必须走原生实现（dialog_darwin.go）：Wails 的
// OpenFile*Dialog 把 sheet 挂在被掏空的宿主窗口上，会弹出半透明占位窗口。
// 其他平台没有这个问题（Windows 用 IFileDialog，宿主窗口是正常窗口），
// 调用方拿到 ErrUnsupported 后应回退到 Wails 的对话框。
func RunOpenDialog(opts DialogOptions) (string, error) {
	_ = opts
	return "", ErrUnsupported
}

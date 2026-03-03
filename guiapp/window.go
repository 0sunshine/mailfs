package guiapp

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"mailfs/libfs"
)

// NewMainWindow 创建主窗口
func NewMainWindow(a fyne.App) fyne.Window {
	w := a.NewWindow("MailFS  —  邮件文件系统")
	w.Resize(fyne.NewSize(1200, 720))
	w.SetMaster()

	download := NewDownloadPage(libfs.NewMailFileSystem())
	upload := NewUploadPage(libfs.NewMailFileSystem())

	// 注入窗口引用（用于文件对话框和右键菜单）
	download.SetWindow(w)
	upload.SetWindow(w)

	tabs := container.NewAppTabs(
		container.NewTabItemWithIcon("  下载  ", theme.DownloadIcon(), download.Content()),
		container.NewTabItemWithIcon("  上传  ", theme.UploadIcon(), upload.Content()),
	)
	tabs.SetTabLocation(container.TabLocationTop)

	// ── 右上角关闭按钮 ───────────────────────────────
	closeBtn := widget.NewButtonWithIcon("", theme.WindowCloseIcon(), func() {
		w.Close()
	})
	closeBtn.Importance = widget.DangerImportance

	// ── 底部版本信息 ─────────────────────────────────
	ver := widget.NewLabelWithStyle(
		"MailFS v1.0  |  QQ IMAP",
		fyne.TextAlignTrailing,
		fyne.TextStyle{Monospace: true},
	)
	ver.Importance = widget.LowImportance

	// tabs 占中央，关闭按钮钉在右上角，版本信息在底部
	topBar := container.NewBorder(nil, nil, nil, closeBtn)
	root := container.NewBorder(topBar, ver, nil, nil, tabs)

	w.SetContent(root)
	return w
}

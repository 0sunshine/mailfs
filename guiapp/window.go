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

	// 底部状态栏
	ver := widget.NewLabelWithStyle("MailFS v1.0  |  QQ IMAP",
		fyne.TextAlignTrailing,
		fyne.TextStyle{Monospace: true},
	)
	ver.Importance = widget.LowImportance

	root := container.NewBorder(nil, ver, nil, nil, tabs)
	w.SetContent(root)
	return w
}

package guiapp

import (
	"fmt"
	"mailfs/libfs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/sirupsen/logrus"
)

// UploadPage 上传页面
type UploadPage struct {
	fs  *libfs.MailFileSystem
	win fyne.Window

	folderList    *widget.List
	logView       *widget.List
	statusLabel   *widget.Label
	uploadFileBtn *widget.Button
	uploadDirBtn  *widget.Button
	targetLabel   *widget.Label

	// 拖拽提示区域
	dropZone *dropZoneWidget

	// 块级进度
	blockBar   *widget.ProgressBar
	blockLabel *widget.Label

	// 文件级进度（目录上传专用）
	fileBar   *widget.ProgressBar
	fileLabel *widget.Label

	folders   []string
	selFolder string
	logs      []string
	mu        sync.Mutex
}

func NewUploadPage(fs *libfs.MailFileSystem) *UploadPage {
	return &UploadPage{
		fs:   fs,
		logs: make([]string, 0, 200),
	}
}

func (p *UploadPage) SetWindow(w fyne.Window) {
	p.win = w

	// 注册窗口级拖拽回调
	w.SetOnDropped(func(pos fyne.Position, uris []fyne.URI) {
		p.handleDrop(uris)
	})
}

// handleDrop 处理从系统拖入的文件/文件夹
func (p *UploadPage) handleDrop(uris []fyne.URI) {
	if p.selFolder == "" {
		p.setStatus("⚠ 请先在左侧选择目标文件夹，再拖入文件")
		p.addLog("⚠ 拖拽被忽略：尚未选择目标文件夹")
		return
	}

	if len(uris) == 0 {
		return
	}

	go func() {
		for _, uri := range uris {
			path := uri.Path()
			if path == "" {
				continue
			}

			info, err := os.Stat(path)
			if err != nil {
				p.addLog(fmt.Sprintf("✗ 无法访问: %s — %v", path, err))
				continue
			}

			if info.IsDir() {
				p.addLog(fmt.Sprintf("📂 拖入文件夹: %s", filepath.Base(path)))
				p.doUploadDir(path)
			} else {
				p.addLog(fmt.Sprintf("📄 拖入文件: %s", filepath.Base(path)))
				p.doUploadFile(path)
			}
		}
	}()
}

func (p *UploadPage) Content() fyne.CanvasObject {
	// ── 左侧文件夹列表 ──────────────────────────────
	p.folderList = widget.NewList(
		func() int { return len(p.folders) },
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.FolderIcon()),
				widget.NewLabel(""),
			)
		},
		func(i widget.ListItemID, obj fyne.CanvasObject) {
			obj.(*fyne.Container).Objects[1].(*widget.Label).SetText(p.folders[i])
		},
	)
	p.folderList.OnSelected = func(id widget.ListItemID) {
		folder := p.folders[id]
		p.selFolder = folder
		p.targetLabel.SetText(folder)
		p.uploadFileBtn.Enable()
		p.uploadDirBtn.Enable()
		// 更新拖拽区提示
		p.dropZone.SetActive(true)
		go func() {
			if err := p.fs.Enter(folder); err != nil {
				p.setStatus(fmt.Sprintf("进入文件夹失败: %v", err))
			} else {
				p.setStatus(fmt.Sprintf("✓ 目标已就绪: [%s]", folder))
			}
		}()
	}

	refreshBtn := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() {
		go p.loadFolders()
	})
	refreshBtn.Importance = widget.LowImportance

	leftHeader := container.NewBorder(nil, nil, nil, refreshBtn, makeHeaderLabel("📁  目标文件夹"))
	leftPanel := container.NewBorder(leftHeader, nil, nil, nil,
		container.NewVScroll(p.folderList))

	// ── 右侧上传控制区 ──────────────────────────────
	p.targetLabel = widget.NewLabel("(未选择)")
	p.targetLabel.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}

	targetRow := container.NewHBox(widget.NewLabel("上传至:"), p.targetLabel)

	p.uploadFileBtn = widget.NewButtonWithIcon("选择文件上传…", theme.FileIcon(), func() {
		p.pickAndUploadFile()
	})
	p.uploadFileBtn.Importance = widget.HighImportance
	p.uploadFileBtn.Disable()

	p.uploadDirBtn = widget.NewButtonWithIcon("选择文件夹上传…", theme.FolderOpenIcon(), func() {
		p.pickAndUploadDir()
	})
	p.uploadDirBtn.Importance = widget.MediumImportance
	p.uploadDirBtn.Disable()

	// ── 拖拽提示区域 ───────────────────────────────
	p.dropZone = newDropZoneWidget()

	// ── 块级进度 ────────────────────────────────────
	p.blockBar = widget.NewProgressBar()
	p.blockBar.Hide()
	p.blockLabel = widget.NewLabel("")
	p.blockLabel.TextStyle = fyne.TextStyle{Monospace: true}
	p.blockLabel.Hide()

	blockRow := container.NewBorder(nil, nil, p.blockLabel, nil, p.blockBar)

	// ── 文件级进度 ──────────────────────────────────
	p.fileBar = widget.NewProgressBar()
	p.fileBar.Hide()
	p.fileLabel = widget.NewLabel("")
	p.fileLabel.TextStyle = fyne.TextStyle{Monospace: true}
	p.fileLabel.Hide()

	fileRow := container.NewBorder(nil, nil, p.fileLabel, nil, p.fileBar)

	// ── 状态标签 ────────────────────────────────────
	p.statusLabel = widget.NewLabel("请先在左侧选择目标文件夹")
	p.statusLabel.TextStyle = fyne.TextStyle{Monospace: true}

	controlBox := container.NewVBox(
		makeHeaderLabel("⬆  上传控制"),
		widget.NewSeparator(),
		targetRow,
		container.NewGridWithColumns(2, p.uploadFileBtn, p.uploadDirBtn),
		widget.NewSeparator(),
		p.dropZone,
		widget.NewSeparator(),
		blockRow,
		fileRow,
		p.statusLabel,
	)

	// ── 日志区 ──────────────────────────────────────
	p.logView = widget.NewList(
		func() int { return len(p.logs) },
		func() fyne.CanvasObject {
			lbl := widget.NewLabel("")
			lbl.TextStyle = fyne.TextStyle{Monospace: true}
			lbl.Wrapping = fyne.TextWrapWord
			return lbl
		},
		func(i widget.ListItemID, obj fyne.CanvasObject) {
			p.mu.Lock()
			defer p.mu.Unlock()
			idx := len(p.logs) - 1 - i
			if idx >= 0 {
				obj.(*widget.Label).SetText(p.logs[idx])
			}
		},
	)

	clearBtn := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
		p.mu.Lock()
		p.logs = p.logs[:0]
		p.mu.Unlock()
		p.logView.Refresh()
	})
	clearBtn.Importance = widget.LowImportance

	logHeader := container.NewBorder(nil, nil, nil, clearBtn, makeHeaderLabel("📋  上传日志"))
	logBox := container.NewBorder(logHeader, nil, nil, nil,
		container.NewVScroll(p.logView))

	rightPanel := container.NewVSplit(controlBox, logBox)
	rightPanel.SetOffset(0.40)

	split := container.NewHSplit(leftPanel, rightPanel)
	split.SetOffset(0.22)

	go p.loadFolders()
	return split
}

// ─── 文件夹加载 ────────────────────────────────────────────────────────────

func (p *UploadPage) loadFolders() {
	p.setStatus("正在获取文件夹列表…")
	folders, err := p.fs.GetMailboxList()
	if err != nil {
		p.setStatus(fmt.Sprintf("获取文件夹失败: %v", err))
		return
	}
	sort.Strings(folders)
	p.mu.Lock()
	p.folders = folders
	p.mu.Unlock()
	fyne.Do(func() { p.folderList.Refresh() })
	p.setStatus(fmt.Sprintf("共 %d 个文件夹，请选择上传目标", len(folders)))
}

// ─── 选择文件/文件夹并上传 ─────────────────────────────────────────────────

func (p *UploadPage) pickAndUploadFile() {
	if p.win == nil {
		return
	}
	d := dialog.NewFileOpen(func(uc fyne.URIReadCloser, err error) {
		if err != nil || uc == nil {
			return
		}
		uc.Close()
		go p.doUploadFile(uc.URI().Path())
	}, p.win)
	d.Show()
}

func (p *UploadPage) pickAndUploadDir() {
	if p.win == nil {
		return
	}
	d := dialog.NewFolderOpen(func(lu fyne.ListableURI, err error) {
		if err != nil || lu == nil {
			return
		}
		go p.doUploadDir(lu.Path())
	}, p.win)
	d.Show()
}

// ─── 上传执行（带双层进度）────────────────────────────────────────────────

func (p *UploadPage) doUploadFile(path string) {
	name := filepath.Base(path)
	p.addLog(fmt.Sprintf("▶ 开始上传: %s", name))
	p.setStatus(fmt.Sprintf("上传中: %s", name))
	p.setButtons(false)

	// 显示块进度条
	p.showBlockProgress(0, 1)
	p.hideFileProgress()

	err := p.fs.UploadFileWithProgress(path, func(cur, total int64, fn string) {
		p.showBlockProgress(cur, total)
		p.setStatus(fmt.Sprintf("⬆  上传中 [%s]  块 %d / %d", name, cur, total))
	})

	p.hideBlockProgress()

	if err != nil {
		logrus.Errorf("upload file error: %v", err)
		p.addLog(fmt.Sprintf("✗ 上传失败: %s — %v", name, err))
		p.setStatus(fmt.Sprintf("✗ 上传失败: %v", err))
	} else {
		p.addLog(fmt.Sprintf("✓ 上传完成: %s", name))
		p.setStatus(fmt.Sprintf("✓ 上传完成: %s", name))
	}
	p.setButtons(true)
}

func (p *UploadPage) doUploadDir(path string) {
	dirName := filepath.Base(path)
	p.addLog(fmt.Sprintf("▶ 开始上传目录: %s/", dirName))
	p.setStatus(fmt.Sprintf("上传目录: %s/…", dirName))
	p.setButtons(false)

	p.showBlockProgress(0, 1)
	p.showFileProgress(0, 1)

	currentFile := ""
	err := p.fs.UploadDirWithProgress(
		path,
		// 文件级回调
		func(done, total int, fp string) {
			currentFile = filepath.Base(fp)
			if fp == "" {
				currentFile = "(完成)"
			}
			p.showFileProgress(int64(done), int64(total))
			p.setStatus(fmt.Sprintf("⬆  文件 %d/%d  %s", done, total, currentFile))
		},
		// 块级回调
		func(cur, total int64, fn string) {
			p.showBlockProgress(cur, total)
			p.addLog(fmt.Sprintf("  块 %d/%d  %s", cur, total, filepath.Base(fn)))
		},
	)

	p.hideBlockProgress()
	p.hideFileProgress()

	if err != nil {
		logrus.Errorf("upload dir error: %v", err)
		p.addLog(fmt.Sprintf("✗ 目录上传失败: %v", err))
		p.setStatus(fmt.Sprintf("✗ 目录上传失败: %v", err))
	} else {
		p.addLog(fmt.Sprintf("✓ 目录上传完成: %s/", dirName))
		p.setStatus(fmt.Sprintf("✓ 目录上传完成: %s/", dirName))
	}
	p.setButtons(true)
}

// ─── 进度条 helpers ────────────────────────────────────────────────────────

func (p *UploadPage) showBlockProgress(cur, total int64) {
	if total <= 0 {
		total = 1
	}
	v := float64(cur) / float64(total)
	fyne.Do(func() {
		p.blockBar.Show()
		p.blockLabel.Show()
		p.blockBar.SetValue(v)
		p.blockLabel.SetText(fmt.Sprintf("块 %d/%d", cur, total))
	})
}

func (p *UploadPage) hideBlockProgress() {
	fyne.Do(func() {
		p.blockBar.SetValue(0)
		p.blockBar.Hide()
		p.blockLabel.Hide()
	})
}

func (p *UploadPage) showFileProgress(cur, total int64) {
	if total <= 0 {
		total = 1
	}
	v := float64(cur) / float64(total)
	fyne.Do(func() {
		p.fileBar.Show()
		p.fileLabel.Show()
		p.fileBar.SetValue(v)
		p.fileLabel.SetText(fmt.Sprintf("文件 %d/%d", cur, total))
	})
}

func (p *UploadPage) hideFileProgress() {
	fyne.Do(func() {
		p.fileBar.SetValue(0)
		p.fileBar.Hide()
		p.fileLabel.Hide()
	})
}

// ─── 日志 / 状态 ──────────────────────────────────────────────────────────

func (p *UploadPage) addLog(msg string) {
	p.mu.Lock()
	p.logs = append(p.logs, msg)
	n := len(p.logs)
	p.mu.Unlock()
	fyne.Do(func() {
		p.logView.Refresh()
		if n > 0 {
			p.logView.ScrollTo(0)
		}
	})
}

func (p *UploadPage) setStatus(msg string) {
	fyne.Do(func() { p.statusLabel.SetText(msg) })
}

func (p *UploadPage) setButtons(enabled bool) {
	fyne.Do(func() {
		if enabled {
			p.uploadFileBtn.Enable()
			p.uploadDirBtn.Enable()
		} else {
			p.uploadFileBtn.Disable()
			p.uploadDirBtn.Disable()
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// dropZoneWidget — 拖拽提示区域
//
// 在上传控制区显示一个带虚线边框的提示区域，告知用户可以拖拽文件/文件夹。
// 选中目标文件夹后提示文字变为"就绪"状态。
// ─────────────────────────────────────────────────────────────────────────────

type dropZoneWidget struct {
	widget.BaseWidget
	active bool
	label  *widget.Label
	icon   *widget.Icon
	border *canvas.Rectangle
}

func newDropZoneWidget() *dropZoneWidget {
	lbl := widget.NewLabel("请先选择目标文件夹，然后拖拽文件或文件夹到窗口即可上传")
	lbl.Alignment = fyne.TextAlignLeading
	lbl.Wrapping = fyne.TextWrapWord

	icon := widget.NewIcon(theme.UploadIcon())

	border := canvas.NewRectangle(theme.DisabledColor())
	border.StrokeWidth = 2
	border.StrokeColor = theme.DisabledColor()
	border.FillColor = theme.HoverColor()
	border.CornerRadius = 8

	d := &dropZoneWidget{
		active: false,
		label:  lbl,
		icon:   icon,
		border: border,
	}
	d.ExtendBaseWidget(d)
	return d
}

func (d *dropZoneWidget) SetActive(active bool) {
	d.active = active
	fyne.Do(func() {
		if active {
			d.label.SetText("✓ 目标已就绪 — 拖拽文件或文件夹到窗口即可上传")
			d.border.StrokeColor = theme.PrimaryColor()
			d.border.FillColor = theme.SelectionColor()
		} else {
			d.label.SetText("请先选择目标文件夹，然后拖拽文件或文件夹到窗口即可上传")
			d.border.StrokeColor = theme.DisabledColor()
			d.border.FillColor = theme.HoverColor()
		}
		d.border.Refresh()
	})
}

func (d *dropZoneWidget) CreateRenderer() fyne.WidgetRenderer {
	// 图标在左，文字在右，水平排列，文字自动撑满剩余宽度
	content := container.NewBorder(nil, nil, d.icon, nil, d.label)
	return widget.NewSimpleRenderer(
		container.NewStack(d.border, container.NewPadded(content)),
	)
}

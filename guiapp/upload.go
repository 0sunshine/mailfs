package guiapp

import (
	"fmt"
	"mailfs/libfs"
	"path/filepath"
	"sort"
	"sync"

	"fyne.io/fyne/v2"
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
	progressBar   *widget.ProgressBarInfinite
	uploadFileBtn *widget.Button
	uploadDirBtn  *widget.Button
	targetLabel   *widget.Label

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

func (p *UploadPage) SetWindow(w fyne.Window) { p.win = w }

func (p *UploadPage) Content() fyne.CanvasObject {
	// ── 左侧文件夹列表 ──
	p.folderList = widget.NewList(
		func() int { return len(p.folders) },
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.FolderIcon()),
				widget.NewLabel("placeholder"),
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
		go func() {
			if err := p.fs.Enter(folder); err != nil {
				p.setStatus(fmt.Sprintf("进入文件夹失败: %v", err))
			} else {
				p.setStatus(fmt.Sprintf("目标: [%s]  已就绪", folder))
			}
		}()
	}

	refreshBtn := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() {
		go p.loadFolders()
	})
	refreshBtn.Importance = widget.LowImportance

	leftHeader := container.NewBorder(nil, nil, nil, refreshBtn,
		makeHeaderLabel("📁  目标文件夹"),
	)
	leftPanel := container.NewBorder(leftHeader, nil, nil, nil,
		container.NewVScroll(p.folderList),
	)

	// ── 右侧上传控制区 ──
	p.targetLabel = widget.NewLabel("(未选择)")
	p.targetLabel.TextStyle = fyne.TextStyle{Bold: true, Monospace: true}

	targetRow := container.NewHBox(
		widget.NewLabel("上传至:"),
		p.targetLabel,
	)

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

	p.progressBar = widget.NewProgressBarInfinite()
	p.progressBar.Hide()

	p.statusLabel = widget.NewLabel("请先在左侧选择目标文件夹")
	p.statusLabel.TextStyle = fyne.TextStyle{Monospace: true}

	controlBox := container.NewVBox(
		makeHeaderLabel("⬆  上传控制"),
		widget.NewSeparator(),
		targetRow,
		container.NewGridWithColumns(2, p.uploadFileBtn, p.uploadDirBtn),
		p.progressBar,
		p.statusLabel,
	)

	// ── 日志区 ──
	p.logView = widget.NewList(
		func() int { return len(p.logs) },
		func() fyne.CanvasObject {
			lbl := widget.NewLabel("")
			lbl.TextStyle = fyne.TextStyle{Monospace: true}
			lbl.Wrapping = fyne.TextWrapWord
			return lbl
		},
		func(i widget.ListItemID, obj fyne.CanvasObject) {
			// 倒序：最新日志在上
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

	logHeader := container.NewBorder(nil, nil, nil, clearBtn,
		makeHeaderLabel("📋  上传日志"),
	)
	logBox := container.NewBorder(logHeader, nil, nil, nil,
		container.NewVScroll(p.logView),
	)

	rightPanel := container.NewVSplit(controlBox, logBox)
	rightPanel.SetOffset(0.35)

	split := container.NewHSplit(leftPanel, rightPanel)
	split.SetOffset(0.22)

	go p.loadFolders()
	return split
}

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
	p.folderList.Refresh()
	p.setStatus(fmt.Sprintf("共 %d 个文件夹，请选择上传目标", len(folders)))
}

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

func (p *UploadPage) doUploadFile(path string) {
	name := filepath.Base(path)
	p.addLog(fmt.Sprintf("▶ 开始上传: %s", name))
	p.setStatus(fmt.Sprintf("上传中: %s", name))
	p.showProgress(true)
	p.uploadFileBtn.Disable()
	p.uploadDirBtn.Disable()

	if err := p.fs.UploadFile(path); err != nil {
		logrus.Errorf("upload file error: %v", err)
		p.addLog(fmt.Sprintf("✗ 上传失败: %s — %v", name, err))
		p.setStatus(fmt.Sprintf("上传失败: %v", err))
	} else {
		p.addLog(fmt.Sprintf("✓ 上传完成: %s", name))
		p.setStatus(fmt.Sprintf("✓ 上传完成: %s", name))
	}

	p.uploadFileBtn.Enable()
	p.uploadDirBtn.Enable()
	p.showProgress(false)
}

func (p *UploadPage) doUploadDir(path string) {
	name := filepath.Base(path)
	p.addLog(fmt.Sprintf("▶ 开始上传目录: %s/", name))
	p.setStatus(fmt.Sprintf("上传目录中: %s/…", name))
	p.showProgress(true)
	p.uploadFileBtn.Disable()
	p.uploadDirBtn.Disable()

	if err := p.fs.UploadDir(path); err != nil {
		logrus.Errorf("upload dir error: %v", err)
		p.addLog(fmt.Sprintf("✗ 目录上传失败: %v", err))
		p.setStatus(fmt.Sprintf("目录上传失败: %v", err))
	} else {
		p.addLog(fmt.Sprintf("✓ 目录上传完成: %s/", name))
		p.setStatus(fmt.Sprintf("✓ 目录上传完成: %s/", name))
	}

	p.uploadFileBtn.Enable()
	p.uploadDirBtn.Enable()
	p.showProgress(false)
}

func (p *UploadPage) addLog(msg string) {
	p.mu.Lock()
	p.logs = append(p.logs, msg)
	p.mu.Unlock()
	p.logView.Refresh()
	if len(p.logs) > 0 {
		p.logView.ScrollTo(0)
	}
}

func (p *UploadPage) setStatus(msg string) { p.statusLabel.SetText(msg) }

func (p *UploadPage) showProgress(show bool) {
	if show {
		p.progressBar.Show()
		p.progressBar.Start()
	} else {
		p.progressBar.Stop()
		p.progressBar.Hide()
	}
}

package guiapp

import (
	"fmt"
	"mailfs/libfs"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/sirupsen/logrus"
)

const downloadPageSize = 50

// ── DownloadPage ─────────────────────────────────────────────────────────────

type DownloadPage struct {
	fs  *libfs.MailFileSystem
	win fyne.Window

	folderList  *widget.List
	fileTable   *RightClickTable
	statusLabel *widget.Label
	progressBar *widget.ProgressBarInfinite
	syncBtn     *widget.Button
	prevBtn     *widget.Button
	nextBtn     *widget.Button
	pageLabel   *widget.Label

	folders     []string
	selFolder   string
	allFiles    []libfs.CacheFile
	pageFiles   []libfs.CacheFile
	currentPage int
	totalPages  int
	mu          sync.Mutex
}

func NewDownloadPage(fs *libfs.MailFileSystem) *DownloadPage {
	return &DownloadPage{
		fs:          fs,
		currentPage: 1,
		totalPages:  1,
	}
}

func (p *DownloadPage) SetWindow(w fyne.Window) { p.win = w }

func (p *DownloadPage) Content() fyne.CanvasObject {
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
		go p.selectFolder(p.folders[id])
	}

	refreshFolderBtn := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() {
		go p.loadFolders()
	})
	refreshFolderBtn.Importance = widget.LowImportance

	leftHeader := container.NewBorder(nil, nil, nil, refreshFolderBtn,
		makeHeaderLabel("📁  邮箱文件夹"),
	)
	leftPanel := container.NewBorder(leftHeader, nil, nil, nil,
		container.NewVScroll(p.folderList),
	)

	// ── 工具栏 ──
	p.syncBtn = widget.NewButtonWithIcon("同步缓存", theme.ViewRefreshIcon(), func() {
		go p.syncCache()
	})
	p.syncBtn.Importance = widget.HighImportance

	p.prevBtn = widget.NewButtonWithIcon("", theme.NavigateBackIcon(), func() {
		if p.currentPage > 1 {
			p.currentPage--
			p.refreshTable()
		}
	})
	p.nextBtn = widget.NewButtonWithIcon("", theme.NavigateNextIcon(), func() {
		if p.currentPage < p.totalPages {
			p.currentPage++
			p.refreshTable()
		}
	})
	p.pageLabel = widget.NewLabel("第 1 / 1 页")
	p.pageLabel.TextStyle = fyne.TextStyle{Monospace: true}

	p.statusLabel = widget.NewLabel("就绪  |  请选择文件夹")
	p.statusLabel.TextStyle = fyne.TextStyle{Monospace: true}

	p.progressBar = widget.NewProgressBarInfinite()
	p.progressBar.Hide()

	toolbar := container.NewBorder(nil, nil,
		container.NewHBox(
			p.syncBtn,
			widget.NewSeparator(),
			p.prevBtn, p.pageLabel, p.nextBtn,
		),
		nil,
		p.statusLabel,
	)

	// ── 表格 ──
	headers := []string{"文件路径", "块数", "文件 MD5", "邮件夹"}
	colWidths := []float32{480, 60, 270, 220}

	p.fileTable = NewRightClickTable(
		headers, colWidths,
		func() int { return len(p.pageFiles) },
		func(row, col int) string {
			if row >= len(p.pageFiles) {
				return ""
			}
			f := p.pageFiles[row]
			switch col {
			case 0:
				return f.LocalPath
			case 1:
				return fmt.Sprintf("%d", f.BlockCount)
			case 2:
				return f.FileMD5
			case 3:
				return f.MailFolder
			}
			return ""
		},
		func(row int) {
			if row < len(p.pageFiles) {
				p.showDownloadMenu(p.pageFiles[row])
			}
		},
	)

	rightPanel := container.NewBorder(toolbar, p.progressBar, nil, nil, p.fileTable)

	split := container.NewHSplit(leftPanel, rightPanel)
	split.SetOffset(0.22)

	go p.loadFolders()
	return split
}

func (p *DownloadPage) loadFolders() {
	p.setStatus("正在获取文件夹列表…")
	folders, err := p.fs.GetMailboxList()
	if err != nil {
		p.setStatus(fmt.Sprintf("获取文件夹失败: %v", err))
		return
	}
	p.mu.Lock()
	p.folders = folders
	p.mu.Unlock()
	p.folderList.Refresh()
	p.setStatus(fmt.Sprintf("共 %d 个文件夹，请选择", len(folders)))
}

func (p *DownloadPage) selectFolder(folder string) {
	p.selFolder = folder
	p.currentPage = 1
	p.setStatus(fmt.Sprintf("正在进入 [%s]…", folder))
	p.showProgress(true)

	if err := p.fs.Enter(folder); err != nil {
		p.setStatus(fmt.Sprintf("进入失败: %v", err))
		p.showProgress(false)
		return
	}

	files, err := p.fs.GetCacheFiles()
	if err != nil {
		p.setStatus(fmt.Sprintf("加载文件失败: %v", err))
		p.showProgress(false)
		return
	}

	p.mu.Lock()
	p.allFiles = files
	p.totalPages = (len(files) + downloadPageSize - 1) / downloadPageSize
	if p.totalPages == 0 {
		p.totalPages = 1
	}
	p.mu.Unlock()

	p.refreshTable()
	p.showProgress(false)
	p.setStatus(fmt.Sprintf("[%s]  共 %d 个文件  |  右键行可下载", folder, len(files)))
}

func (p *DownloadPage) syncCache() {
	if p.selFolder == "" {
		p.setStatus("请先选择文件夹")
		return
	}
	p.syncBtn.Disable()
	p.setStatus(fmt.Sprintf("正在同步 [%s]，请稍候…", p.selFolder))
	p.showProgress(true)

	if err := p.fs.CacheCurrDir(); err != nil {
		p.setStatus(fmt.Sprintf("同步失败: %v", err))
	} else {
		p.setStatus("同步完成，正在刷新…")
		p.selectFolder(p.selFolder)
	}

	p.syncBtn.Enable()
	p.showProgress(false)
}

func (p *DownloadPage) showDownloadMenu(f libfs.CacheFile) {
	if p.win == nil {
		return
	}
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("⬇  下载  "+shortPath(f.LocalPath), func() {
			go p.downloadFile(f)
		}),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("📋  复制路径", func() {
			p.win.Clipboard().SetContent(f.LocalPath)
			p.setStatus("已复制: " + f.LocalPath)
		}),
		fyne.NewMenuItem("🔍  查看 MD5", func() {
			p.setStatus("MD5: " + f.FileMD5)
		}),
	)
	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(p.fileTable)
	widget.ShowPopUpMenuAtPosition(menu, p.win.Canvas(), pos)
}

func (p *DownloadPage) downloadFile(f libfs.CacheFile) {
	p.showProgress(true)
	p.setStatus(fmt.Sprintf("⬇  下载中: %s", shortPath(f.LocalPath)))
	if err := p.fs.DownloadCacheFile(f); err != nil {
		logrus.Errorf("download error: %v", err)
		p.setStatus(fmt.Sprintf("✗ 下载失败: %v", err))
	} else {
		p.setStatus(fmt.Sprintf("✓ 下载完成: %s", shortPath(f.LocalPath)))
	}
	p.showProgress(false)
}

func (p *DownloadPage) refreshTable() {
	p.mu.Lock()
	start := (p.currentPage - 1) * downloadPageSize
	end := start + downloadPageSize
	if end > len(p.allFiles) {
		end = len(p.allFiles)
	}
	p.pageFiles = p.allFiles[start:end]
	p.mu.Unlock()

	p.fileTable.Refresh()
	p.pageLabel.SetText(fmt.Sprintf("第 %d / %d 页", p.currentPage, p.totalPages))

	if p.currentPage <= 1 {
		p.prevBtn.Disable()
	} else {
		p.prevBtn.Enable()
	}
	if p.currentPage >= p.totalPages {
		p.nextBtn.Disable()
	} else {
		p.nextBtn.Enable()
	}
}

func (p *DownloadPage) setStatus(msg string) { p.statusLabel.SetText(msg) }

func (p *DownloadPage) showProgress(show bool) {
	if show {
		p.progressBar.Show()
		p.progressBar.Start()
	} else {
		p.progressBar.Stop()
		p.progressBar.Hide()
	}
}

func shortPath(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) > 2 {
		return "…/" + strings.Join(parts[len(parts)-2:], "/")
	}
	return path
}

// ── RightClickTable ──────────────────────────────────────────────────────────
// 包装 widget.Table，通过 BaseWidget + TappedSecondary 实现右键菜单

type RightClickTable struct {
	widget.BaseWidget

	inner        *widget.Table
	onRightClick func(row int)
	lastRow      int
}

func NewRightClickTable(
	headers []string,
	colWidths []float32,
	rowCount func() int,
	cellValue func(row, col int) string,
	onRightClick func(row int),
) *RightClickTable {
	rt := &RightClickTable{
		onRightClick: onRightClick,
		lastRow:      -1,
	}

	cols := len(headers)
	t := widget.NewTable(
		func() (int, int) { return rowCount() + 1, cols },
		func() fyne.CanvasObject {
			lbl := widget.NewLabel("")
			lbl.TextStyle = fyne.TextStyle{Monospace: true}
			lbl.Truncation = fyne.TextTruncateEllipsis
			return lbl
		},
		func(id widget.TableCellID, obj fyne.CanvasObject) {
			lbl := obj.(*widget.Label)
			if id.Row == 0 {
				lbl.TextStyle = fyne.TextStyle{Bold: true}
				if id.Col < len(headers) {
					lbl.SetText(headers[id.Col])
				}
				return
			}
			lbl.TextStyle = fyne.TextStyle{Monospace: true}
			lbl.SetText(cellValue(id.Row-1, id.Col))
		},
	)
	t.SetRowHeight(0, 30)
	for i, w := range colWidths {
		t.SetColumnWidth(i, w)
	}
	t.OnSelected = func(id widget.TableCellID) {
		if id.Row > 0 {
			rt.lastRow = id.Row - 1
		}
	}

	rt.inner = t
	rt.ExtendBaseWidget(rt)
	return rt
}

// CreateRenderer 让 Fyne 渲染内部 table
func (rt *RightClickTable) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(rt.inner)
}

// TappedSecondary 捕获右键/长按
func (rt *RightClickTable) TappedSecondary(_ *fyne.PointEvent) {
	if rt.lastRow >= 0 && rt.onRightClick != nil {
		rt.onRightClick(rt.lastRow)
	}
}

func (rt *RightClickTable) Refresh() {
	rt.inner.Refresh()
	rt.BaseWidget.Refresh()
}

func (rt *RightClickTable) MinSize() fyne.Size {
	return rt.inner.MinSize()
}

// ── shared helpers ────────────────────────────────────────────────────────────

func makeHeaderLabel(text string) fyne.CanvasObject {
	lbl := widget.NewLabelWithStyle(text, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	line := canvas.NewLine(theme.PrimaryColor())
	line.StrokeWidth = 1.5
	return container.NewVBox(lbl, line)
}

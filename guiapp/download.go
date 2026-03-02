package guiapp

import (
	"fmt"
	"mailfs/libfs"
	"sort"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/sirupsen/logrus"
)

const downloadPageSize = 50

// ─────────────────────────────────────────────────────────────────────────────
// DownloadPage
// ─────────────────────────────────────────────────────────────────────────────

type DownloadPage struct {
	fs  *libfs.MailFileSystem
	win fyne.Window

	folderList *widget.List
	pathTree   *widget.Tree
	fileTable  *fileTableWidget

	statusLabel *widget.Label
	blockBar    *widget.ProgressBar
	blockLabel  *widget.Label
	syncBtn     *widget.Button
	checkBtn    *widget.Button
	prevBtn     *widget.Button
	nextBtn     *widget.Button
	pageLabel   *widget.Label

	folders       []string
	selFolder     string
	allFiles      []libfs.CacheFile
	filteredFiles []libfs.CacheFile
	pageFiles     []libfs.CacheFile
	currentPage   int
	totalPages    int

	// 路径树: 父节点ID → 子节点ID列表
	// 节点ID = 路径各层用 "/" 拼接, 根节点 ID = ""
	// 例: "", "C:", "C:/Users", "C:/Users/Alice"
	treeChildren map[string][]string // 目录节点
	treeDirs     map[string]bool     // 是否是目录（有子目录或含文件）

	mu sync.Mutex

	// 最近选中的文件夹名和树节点路径，用于复制
	lastSelFolder string
	lastSelNode   string
}

func NewDownloadPage(fs *libfs.MailFileSystem) *DownloadPage {
	return &DownloadPage{
		fs:           fs,
		currentPage:  1,
		totalPages:   1,
		treeChildren: make(map[string][]string),
		treeDirs:     make(map[string]bool),
	}
}

func (p *DownloadPage) SetWindow(w fyne.Window) { p.win = w }

// ─────────────────────────────────────────────────────────────────────────────
// Content — 构建页面布局
// ─────────────────────────────────────────────────────────────────────────────

func (p *DownloadPage) Content() fyne.CanvasObject {
	// ── 左上：邮箱文件夹列表 ─────────────────────────
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
		p.lastSelFolder = folder
		go p.selectFolder(folder)
	}

	refreshBtn := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), func() {
		go p.loadFolders()
	})
	refreshBtn.Importance = widget.LowImportance

	copyFolderBtn := widget.NewButtonWithIcon("", theme.ContentCopyIcon(), func() {
		if p.win != nil && p.lastSelFolder != "" {
			p.win.Clipboard().SetContent(p.lastSelFolder)
			p.setStatus("已复制文件夹: " + p.lastSelFolder)
		}
	})
	copyFolderBtn.Importance = widget.LowImportance
	folderHeader := container.NewBorder(nil, nil, nil,
		container.NewHBox(copyFolderBtn, refreshBtn),
		makeHeaderLabel("📁 邮箱文件夹"),
	)
	// folderList 固定显示区域：给一个明确的 MinSize，避免撑开 HSplit
	folderScroll := container.NewVScroll(p.folderList)
	folderScroll.SetMinSize(fyne.NewSize(0, 120))
	folderPanel := container.NewBorder(folderHeader, nil, nil, nil, folderScroll)

	// ── 左下：路径树 ─────────────────────────────────
	p.pathTree = widget.NewTree(
		// childUIDs: 返回某节点的直接子节点 ID 列表
		func(id widget.TreeNodeID) []widget.TreeNodeID {
			p.mu.Lock()
			defer p.mu.Unlock()
			children, ok := p.treeChildren[id]
			if !ok {
				return nil
			}
			result := make([]widget.TreeNodeID, len(children))
			copy(result, children)
			return result
		},
		// isBranch: 该节点是否可展开（有子目录）
		func(id widget.TreeNodeID) bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			children, ok := p.treeChildren[id]
			return ok && len(children) > 0
		},
		// createNode: 创建节点渲染对象
		func(branch bool) fyne.CanvasObject {
			icon := widget.NewIcon(theme.FolderIcon())
			lbl := widget.NewLabel("")
			lbl.TextStyle = fyne.TextStyle{Monospace: true}
			lbl.Wrapping = fyne.TextWrapOff
			return container.NewBorder(nil, nil, icon, nil, lbl)
		},
		// updateNode: 用数据填充节点
		func(id widget.TreeNodeID, branch bool, obj fyne.CanvasObject) {
			box := obj.(*fyne.Container)
			iconW := box.Objects[1].(*widget.Icon)
			lblW := box.Objects[0].(*widget.Label)
			lblW.SetText(id)
			if branch {
				iconW.SetResource(theme.FolderIcon())
			} else {
				iconW.SetResource(theme.FolderOpenIcon())
			}
		},
	)
	p.pathTree.OnSelected = func(id widget.TreeNodeID) {
		p.lastSelNode = id
		p.filterByPath(id)
	}

	copyNodeBtn := widget.NewButtonWithIcon("", theme.ContentCopyIcon(), func() {
		if p.win != nil && p.lastSelNode != "" {
			p.win.Clipboard().SetContent(p.lastSelNode)
			p.setStatus("已复制路径: " + p.lastSelNode)
		}
	})
	copyNodeBtn.Importance = widget.LowImportance
	treeHeader := container.NewBorder(nil, nil, nil, copyNodeBtn, makeHeaderLabel("🌲 文件路径树"))
	treeScroll := container.NewVScroll(p.pathTree)
	treeScroll.SetMinSize(fyne.NewSize(0, 100)) // 固定最小高度，防止 refresh 时撑开布局
	treePanel := container.NewBorder(treeHeader, nil, nil, nil, treeScroll)

	// 用 NewBorder 替代 VSplit：文件夹列表固定在顶部，路径树占据剩余空间。
	// 这样 pathTree.Refresh() 不会导致整体布局的 offset 被重算。
	leftPanel := container.NewBorder(folderPanel, nil, nil, nil, treePanel)

	// ── 工具栏 ───────────────────────────────────────
	p.syncBtn = widget.NewButtonWithIcon("同步缓存", theme.ViewRefreshIcon(), func() {
		go p.syncCache()
	})
	p.syncBtn.Importance = widget.HighImportance

	p.checkBtn = widget.NewButtonWithIcon("完整性检测", theme.WarningIcon(), func() {
		go p.runIntegrityCheck()
	})
	p.checkBtn.Importance = widget.MediumImportance

	p.prevBtn = widget.NewButtonWithIcon("", theme.NavigateBackIcon(), func() {
		if p.currentPage > 1 {
			p.currentPage--
			p.applyPage()
		}
	})
	p.nextBtn = widget.NewButtonWithIcon("", theme.NavigateNextIcon(), func() {
		if p.currentPage < p.totalPages {
			p.currentPage++
			p.applyPage()
		}
	})
	p.pageLabel = widget.NewLabel("第 1 / 1 页")
	p.pageLabel.TextStyle = fyne.TextStyle{Monospace: true}

	p.statusLabel = widget.NewLabel("就绪  |  请选择文件夹")
	p.statusLabel.TextStyle = fyne.TextStyle{Monospace: true}

	p.blockBar = widget.NewProgressBar()
	p.blockBar.Hide()
	p.blockLabel = widget.NewLabel("")
	p.blockLabel.TextStyle = fyne.TextStyle{Monospace: true}
	p.blockLabel.Hide()

	toolbar := container.NewBorder(nil, nil,
		container.NewHBox(
			p.syncBtn, p.checkBtn,
			widget.NewSeparator(),
			p.prevBtn, p.pageLabel, p.nextBtn,
		),
		nil,
		p.statusLabel,
	)
	progressRow := container.NewBorder(nil, nil, p.blockLabel, nil, p.blockBar)

	// ── 文件表格 ─────────────────────────────────────
	headers := []string{"文件名", "块数", "文件 MD5", "完整路径"}
	colWidths := []float32{220, 60, 260, 420}

	p.fileTable = newFileTableWidget(
		headers,
		colWidths,
		func() int { return len(p.pageFiles) },
		func(row, col int) string {
			if row >= len(p.pageFiles) {
				return ""
			}
			f := p.pageFiles[row]
			switch col {
			case 0:
				return lastSegment(f.LocalPath)
			case 1:
				return fmt.Sprintf("%d", f.BlockCount)
			case 2:
				return f.FileMD5
			case 3:
				return f.LocalPath
			}
			return ""
		},
		// 右键回调：row 是数据行（0起），pos 是屏幕绝对坐标
		func(row int, pos fyne.Position) {
			if row < len(p.pageFiles) {
				p.showDownloadMenu(p.pageFiles[row], pos)
			}
		},
		p.win,
	)

	rightPanel := container.NewBorder(
		container.NewVBox(toolbar, progressRow),
		nil, nil, nil,
		p.fileTable,
	)

	mainSplit := container.NewHSplit(leftPanel, rightPanel)
	mainSplit.SetOffset(0.25)

	go p.loadFolders()
	return mainSplit
}

// ─────────────────────────────────────────────────────────────────────────────
// 数据加载
// ─────────────────────────────────────────────────────────────────────────────

func (p *DownloadPage) loadFolders() {
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
	fyne.Do(func() {
		p.folderList.Refresh()
	})
	p.setStatus(fmt.Sprintf("共 %d 个文件夹，请选择", len(folders)))
}

func (p *DownloadPage) selectFolder(folder string) {
	p.selFolder = folder
	p.currentPage = 1
	p.setStatus(fmt.Sprintf("正在进入 [%s]…", folder))

	if err := p.fs.Enter(folder); err != nil {
		p.setStatus(fmt.Sprintf("进入失败: %v", err))
		return
	}

	files, err := p.fs.GetCacheFiles()
	if err != nil {
		p.setStatus(fmt.Sprintf("加载文件失败: %v", err))
		return
	}

	p.mu.Lock()
	p.allFiles = files
	p.filteredFiles = files
	p.mu.Unlock()

	p.rebuildTree(files)
	p.resetPagination(files)
	p.setStatus(fmt.Sprintf("[%s]  共 %d 个文件  |  右键行可下载", folder, len(files)))
}

func (p *DownloadPage) syncCache() {
	if p.selFolder == "" {
		p.setStatus("请先选择文件夹")
		return
	}
	fyne.Do(func() { p.syncBtn.Disable() })
	p.setStatus(fmt.Sprintf("正在同步 [%s]…", p.selFolder))
	if err := p.fs.CacheCurrDir(); err != nil {
		p.setStatus(fmt.Sprintf("同步失败: %v", err))
	} else {
		p.setStatus("同步完成，正在刷新…")
		p.selectFolder(p.selFolder)
	}
	fyne.Do(func() { p.syncBtn.Enable() })
}

// ─────────────────────────────────────────────────────────────────────────────
// 路径树构建
// ─────────────────────────────────────────────────────────────────────────────

// rebuildTree 根据 CacheFile 列表构建 widget.Tree 所需的节点映射。
//
// 规则：
//   - 节点 ID = 用 "/" 拼接的路径段（Windows 路径先统一为 "/"）
//   - 根节点 ID = ""（widget.Tree 约定）
//   - 只有目录节点（非叶）进入 treeChildren；文件本身不作为树节点
//   - 叶目录（直接含有文件的最深目录）也保留在 treeChildren 中，子列表为空
//
// 示例 LocalPath: "/home/alice/videos/foo.mp4"
//
//	→ 路径段: ["", "home", "alice", "videos", "foo.mp4"]
//	→ 目录层: "" → "home" → "home/alice" → "home/alice/videos"
func (p *DownloadPage) rebuildTree(files []libfs.CacheFile) {
	// children: 父目录ID → []子目录ID（有序）
	children := make(map[string][]string)
	// 用 set 去重
	added := make(map[string]bool)

	// 确保根节点存在
	children[""] = []string{}
	added[""] = true

	for _, f := range files {
		lp := normalizePath(f.LocalPath)
		if lp == "" {
			continue
		}

		parts := strings.Split(lp, "/")
		// parts 最后一个是文件名，我们只处理目录部分
		// 如果只有一个段（文件在根目录），目录就是根 ""
		dirDepth := len(parts) - 1 // 目录层数

		parentID := ""
		for d := 0; d < dirDepth; d++ {
			nodeID := strings.Join(parts[:d+1], "/")
			if !added[nodeID] {
				added[nodeID] = true
				children[nodeID] = []string{} // 初始化为目录节点
				// 追加到父节点的子列表
				children[parentID] = appendUniq(children[parentID], nodeID)
			}
			parentID = nodeID
		}

		// 如果文件直接在根目录（dirDepth==0），确保根节点子列表存在
		// （根节点已在上方初始化，无需额外操作）
	}

	// 对每层子列表排序
	for k := range children {
		sort.Strings(children[k])
	}

	p.mu.Lock()
	p.treeChildren = children
	p.mu.Unlock()

	fyne.Do(func() {
		p.pathTree.Refresh()
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 路径过滤
// ─────────────────────────────────────────────────────────────────────────────

func (p *DownloadPage) filterByPath(nodeID string) {
	p.mu.Lock()
	all := p.allFiles
	p.mu.Unlock()

	var filtered []libfs.CacheFile
	if nodeID == "" {
		filtered = all
	} else {
		prefix := nodeID + "/"
		for _, f := range all {
			lp := normalizePath(f.LocalPath) + "/"
			if strings.HasPrefix(lp, prefix) {
				filtered = append(filtered, f)
			}
		}
	}

	p.mu.Lock()
	p.filteredFiles = filtered
	p.mu.Unlock()

	p.currentPage = 1
	p.resetPagination(filtered)
	// 状态栏只显示选中节点的最后一段名称，避免路径中的 "/" 造成视觉混乱
	displayName := nodeID
	if idx := strings.LastIndex(nodeID, "/"); idx >= 0 {
		displayName = nodeID[idx+1:]
	}
	if displayName == "" {
		displayName = "(全部)"
	}
	p.setStatus(fmt.Sprintf("目录 [%s]  显示 %d 个文件  |  右键行可下载", displayName, len(filtered)))
}

// ─────────────────────────────────────────────────────────────────────────────
// 分页
// ─────────────────────────────────────────────────────────────────────────────

func (p *DownloadPage) resetPagination(files []libfs.CacheFile) {
	p.mu.Lock()
	p.totalPages = (len(files) + downloadPageSize - 1) / downloadPageSize
	if p.totalPages == 0 {
		p.totalPages = 1
	}
	p.mu.Unlock()
	p.applyPage()
}

func (p *DownloadPage) applyPage() {
	p.mu.Lock()
	src := p.filteredFiles
	start := (p.currentPage - 1) * downloadPageSize
	end := start + downloadPageSize
	if start > len(src) {
		start = len(src)
	}
	if end > len(src) {
		end = len(src)
	}
	p.pageFiles = src[start:end]
	p.mu.Unlock()

	fyne.Do(func() {
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
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 下载（含块级进度）
// ─────────────────────────────────────────────────────────────────────────────

func (p *DownloadPage) showDownloadMenu(f libfs.CacheFile, pos fyne.Position) {
	if p.win == nil {
		return
	}
	name := lastSegment(f.LocalPath)
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("⬇  下载  "+name, func() {
			go p.downloadFile(f)
		}),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("📋  复制完整路径", func() {
			p.win.Clipboard().SetContent(f.LocalPath)
			p.setStatus("已复制: " + f.LocalPath)
		}),
		fyne.NewMenuItem("🔍  查看 MD5", func() {
			p.setStatus(fmt.Sprintf("MD5: %s  块数: %d", f.FileMD5, f.BlockCount))
		}),
	)
	widget.ShowPopUpMenuAtPosition(menu, p.win.Canvas(), pos)
}

func (p *DownloadPage) downloadFile(f libfs.CacheFile) {
	name := lastSegment(f.LocalPath)
	p.setStatus(fmt.Sprintf("⬇  准备下载: %s", name))
	p.showBlockProgress(0, f.BlockCount)

	err := p.fs.DownloadCacheFileWithProgress(f, func(cur, total int64, _ string) {
		p.showBlockProgress(cur, total)
		p.setStatus(fmt.Sprintf("⬇  下载中 [%s]  块 %d/%d", name, cur, total))
	})

	p.hideBlockProgress()
	if err != nil {
		logrus.Errorf("download error: %v", err)
		p.setStatus(fmt.Sprintf("✗ 下载失败 [%s]: %v", name, err))
	} else {
		p.setStatus(fmt.Sprintf("✓ 下载完成: %s", name))
	}
}

func (p *DownloadPage) showBlockProgress(cur, total int64) {
	if total <= 0 {
		total = 1
	}
	v := float64(cur) / float64(total)
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	fyne.Do(func() {
		p.blockBar.Show()
		p.blockLabel.Show()
		p.blockBar.SetValue(v)
		p.blockLabel.SetText(fmt.Sprintf("块 %d/%d", cur, total))
	})
}

func (p *DownloadPage) hideBlockProgress() {
	fyne.Do(func() {
		p.blockBar.SetValue(0)
		p.blockBar.Hide()
		p.blockLabel.Hide()
	})
}

func (p *DownloadPage) setStatus(msg string) {
	fyne.Do(func() {
		p.statusLabel.SetText(msg)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 完整性检测
// ─────────────────────────────────────────────────────────────────────────────

func (p *DownloadPage) runIntegrityCheck() {
	if p.selFolder == "" {
		p.setStatus("请先选择文件夹")
		return
	}
	fyne.Do(func() { p.checkBtn.Disable() })
	p.setStatus(fmt.Sprintf("正在检测 [%s] 完整性…", p.selFolder))

	results, err := libfs.CheckIntegrity(p.selFolder)
	if err != nil {
		p.setStatus(fmt.Sprintf("检测失败: %v", err))
		p.checkBtn.Enable()
		return
	}

	total := len(results)
	var broken []libfs.IntegrityResult
	for _, r := range results {
		if !r.OK {
			broken = append(broken, r)
		}
	}

	if len(broken) == 0 {
		p.setStatus(fmt.Sprintf("✓ 完整性检测通过：共 %d 个文件，全部完整", total))
		showInfoDialog(p.win, "完整性检测",
			fmt.Sprintf("✅  全部 %d 个文件完整，块数一致。", total))
		fyne.Do(func() { p.checkBtn.Enable() })
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("⚠  发现 %d / %d 个文件块数不一致：\n\n", len(broken), total))
	for _, r := range broken {
		sb.WriteString(fmt.Sprintf(
			"  %-50s  期望 %3d 块  实际 %3d 块\n",
			lastSegment(r.File.LocalPath), r.ExpectedBlocks, r.CachedBlocks,
		))
	}

	p.setStatus(fmt.Sprintf("⚠  检测完成：%d / %d 文件不完整", len(broken), total))
	fyne.Do(func() {
		showIntegrityReport(p.win, "完整性检测报告", sb.String())
		p.checkBtn.Enable()
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// UI 对话框 helpers
// ─────────────────────────────────────────────────────────────────────────────

func showInfoDialog(win fyne.Window, title, msg string) {
	if win == nil {
		return
	}
	lbl := widget.NewLabel(msg)
	lbl.Wrapping = fyne.TextWrapWord
	pop := newPopupDialog(title, container.NewPadded(lbl), win)
	pop.Show()
}

func showIntegrityReport(win fyne.Window, title, report string) {
	if win == nil {
		return
	}
	lbl := widget.NewLabel(report)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	lbl.Wrapping = fyne.TextWrapWord
	scroll := container.NewVScroll(lbl)
	scroll.SetMinSize(fyne.NewSize(640, 300))
	pop := newPopupDialog(title, scroll, win)
	pop.Show()
}

func newPopupDialog(title string, content fyne.CanvasObject, win fyne.Window) *widget.PopUp {
	closeBtn := widget.NewButtonWithIcon("关闭", theme.CancelIcon(), nil)
	hdr := widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	line := canvas.NewLine(theme.PrimaryColor())
	line.StrokeWidth = 1.5

	box := container.NewBorder(
		container.NewVBox(hdr, line),
		container.NewCenter(closeBtn),
		nil, nil,
		content,
	)

	pop := widget.NewModalPopUp(container.NewPadded(box), win.Canvas())
	pop.Resize(fyne.NewSize(700, 440))
	closeBtn.OnTapped = func() { pop.Hide() }
	return pop
}

// ─────────────────────────────────────────────────────────────────────────────
// fileTableWidget — 真正能响应右键的表格
//
// 关键思路：
//   widget.Table 的每个 cell 是独立的 CanvasObject。
//   为了捕获右键，我们让 createCell 返回一个实现了
//   desktop.Mouseable 接口的自定义 widget（tableCell），
//   这样鼠标按下事件就会在 cell 层面被捕获。
//
//   fileTableWidget 本身继承 BaseWidget，成为合法的
//   fyne.CanvasObject，可以直接放入布局容器。
// ─────────────────────────────────────────────────────────────────────────────

type fileTableWidget struct {
	widget.BaseWidget
	table        *widget.Table
	onRightClick func(row int, pos fyne.Position)
}

func newFileTableWidget(
	headers []string,
	colWidths []float32,
	rowCount func() int,
	cellValue func(row, col int) string,
	onRightClick func(row int, pos fyne.Position),
	win fyne.Window,
) *fileTableWidget {
	ft := &fileTableWidget{onRightClick: onRightClick}

	t := widget.NewTable(
		func() (int, int) { return rowCount() + 1, len(headers) },
		// createCell: 返回一个支持右键的 cell widget
		func() fyne.CanvasObject {
			return newTableCell(win)
		},
		// updateCell: 填充数据，并把行号和回调注入 cell
		func(id widget.TableCellID, obj fyne.CanvasObject) {
			c := obj.(*tableCell)
			if id.Row == 0 {
				c.label.TextStyle = fyne.TextStyle{Bold: true}
				if id.Col < len(headers) {
					c.label.SetText(headers[id.Col])
				}
				c.dataRow = -1
				c.cb = nil
				return
			}
			v := cellValue(id.Row-1, id.Col)
			c.label.TextStyle = fyne.TextStyle{Monospace: true}
			c.label.SetText(v)
			c.cellText = v
			c.dataRow = id.Row - 1
			c.cb = onRightClick
		},
	)

	t.SetRowHeight(0, 30)
	for i, w := range colWidths {
		t.SetColumnWidth(i, w)
	}

	ft.table = t
	ft.ExtendBaseWidget(ft)
	return ft
}

// CreateRenderer 让 Fyne 把内部 table 渲染出来
func (ft *fileTableWidget) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(ft.table)
}

// ─────────────────────────────────────────────────────────────────────────────
// tableCell — 实现 desktop.Mouseable 的 cell 组件
// ─────────────────────────────────────────────────────────────────────────────

type tableCell struct {
	widget.BaseWidget
	label    *widget.Label
	dataRow  int // -1 表示表头
	cb       func(row int, pos fyne.Position)
	cellText string // 当前单元格文字，右键复制用
	win      fyne.Window
}

func newTableCell(win fyne.Window) *tableCell {
	lbl := widget.NewLabel("")
	lbl.Truncation = fyne.TextTruncateEllipsis
	c := &tableCell{
		label:   lbl,
		dataRow: -1,
		win:     win,
	}
	c.ExtendBaseWidget(c)
	return c
}

func (c *tableCell) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(c.label)
}

// MouseDown 实现 desktop.Mouseable
// 右键时：若有 cb（行操作回调）先弹复制菜单，再通过 cb 弹下载菜单会覆盖；
// 改为：把"复制单元格"作为第一个选项直接注入，然后调 cb 触发行菜单替换。
// 实际最佳：cb 负责弹包含复制的完整菜单，tableCell 只传文字。
// 此处简化：右键先弹"复制单元格"菜单；行级下载菜单通过左键选中后右键触发 cb。
func (c *tableCell) MouseDown(ev *desktop.MouseEvent) {
	if ev.Button != desktop.MouseButtonSecondary {
		return
	}
	pos := ev.AbsolutePosition
	if c.win == nil {
		return
	}
	var items []*fyne.MenuItem
	// 复制当前单元格文字
	if c.cellText != "" {
		text := c.cellText
		items = append(items, fyne.NewMenuItem("📋  复制单元格", func() {
			c.win.Clipboard().SetContent(text)
		}))
	}
	// 数据行：追加行级操作（下载等），通过 cb 的菜单项合并进来
	// 由于 cb 会弹独立菜单，这里改为：有复制项时先弹复制菜单；
	// 无文字时直接触发 cb 行菜单
	if c.dataRow >= 0 && c.cb != nil {
		if len(items) > 0 {
			items = append(items, fyne.NewMenuItemSeparator())
		}
		// 把"下载/复制路径/查看MD5"作为子菜单触发点
		row := c.dataRow
		cb := c.cb
		items = append(items, fyne.NewMenuItem("⬇  下载 / 更多操作…", func() {
			cb(row, pos)
		}))
	}
	if len(items) > 0 {
		menu := fyne.NewMenu("", items...)
		widget.ShowPopUpMenuAtPosition(menu, c.win.Canvas(), pos)
	}
}

// MouseUp 必须实现，但无需操作
func (c *tableCell) MouseUp(_ *desktop.MouseEvent) {}

// MinSize 尽量小，让 Table 自己决定尺寸
func (c *tableCell) MinSize() fyne.Size {
	return c.label.MinSize()
}

// ─────────────────────────────────────────────────────────────────────────────
// 共享 helpers
// ─────────────────────────────────────────────────────────────────────────────

// normalizePath 统一路径分隔符，并去除 Windows 盘符前的多余 "/"
// 例: "/G:/foo/bar" → "G:/foo/bar"，"/home/alice" → "home/alice"
func normalizePath(lp string) string {
	// 统一反斜杠为正斜杠
	lp = strings.ReplaceAll(lp, "\\", "/")
	// 去除首部所有多余的 / （处理如 /G:/foo 这类路径）
	lp = strings.TrimLeft(lp, "/")
	return lp
}

// lastSegment 取路径最后一段（文件名或目录名）
func lastSegment(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimRight(path, "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return path
	}
	return path[idx+1:]
}

func appendUniq(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

func makeHeaderLabel(text string) fyne.CanvasObject {
	lbl := widget.NewLabelWithStyle(text, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	line := canvas.NewLine(theme.PrimaryColor())
	line.StrokeWidth = 1.5
	return container.NewVBox(lbl, line)
}

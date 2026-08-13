package ui

import (
	_ "embed"
	"fmt"
	"image/color"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/ncruces/zenity"

	"s3client/internal/awss3"
	"s3client/internal/model"
	"s3client/internal/store"
)

//go:embed icon.ico
var iconBytes []byte

func iconData() []byte { return iconBytes }

// appTheme 是一个深色调的自定义主题，带蓝色主色。
type appTheme struct{}

func (appTheme) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch n {
	case theme.ColorNamePrimary:
		return color.NRGBA{R: 0x3b, G: 0x82, B: 0xf6, A: 0xff}
	case theme.ColorNameBackground:
		return color.NRGBA{R: 0x1a, G: 0x1d, B: 0x24, A: 0xff}
	case theme.ColorNameForeground:
		return color.NRGBA{R: 0xe6, G: 0xe8, B: 0xeb, A: 0xff}
	case theme.ColorNameInputBackground:
		return color.NRGBA{R: 0x24, G: 0x28, B: 0x31, A: 0xff}
	case theme.ColorNameSelection:
		return color.NRGBA{R: 0x3b, G: 0x82, B: 0xf6, A: 0x55}
	case theme.ColorNameButton:
		return color.NRGBA{R: 0x2a, G: 0x2f, B: 0x3a, A: 0xff}
	}
	return theme.DefaultTheme().Color(n, theme.VariantDark)
}

func (appTheme) Font(s fyne.TextStyle) fyne.Resource  { return theme.DefaultTheme().Font(s) }
func (appTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }
func (appTheme) Size(n fyne.ThemeSizeName) float32   { return theme.DefaultTheme().Size(n) }

// revealInExplorer 在系统文件管理器中打开并选中指定文件。
func revealInExplorer(path string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("explorer", "/select,"+filepath.FromSlash(path)).Start()
	case "darwin":
		_ = exec.Command("open", "-R", path).Start()
	default:
		_ = exec.Command("xdg-open", filepath.Dir(path)).Start()
	}
}

// Run 启动应用。
func Run() {
	a := app.New()
	a.Settings().SetTheme(appTheme{})
	a.SetIcon(fyne.NewStaticResource("icon.ico", iconData()))
	w := a.NewWindow("S3 Client")
	w.Resize(fyne.NewSize(960, 600))

	showUnlock(w)

	w.ShowAndRun()
}

// showUnlock 根据初始化状态显示"设置主密码"或"解锁"界面。
func showUnlock(w fyne.Window) {
	w.SetMainMenu(fyne.NewMainMenu())
	initialized, err := store.IsInitialized()
	if err != nil {
		dialog.ShowError(err, w)
	}

	pwd := widget.NewPasswordEntry()
	pwd.SetPlaceHolder("主密码")

	var title, action string
	if initialized {
		title = "输入主密码解锁"
		action = "解锁"
	} else {
		title = "首次使用，请设置主密码"
		action = "设置并进入"
	}

	doUnlock := func() {
		if pwd.Text == "" {
			dialog.ShowInformation("提示", "主密码不能为空", w)
			return
		}
		var s *store.Store
		var err error
		if initialized {
			s, err = store.Open(pwd.Text)
		} else {
			s, err = store.Initialize(pwd.Text)
		}
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		showMain(w, s)
	}
	pwd.OnSubmitted = func(string) { doUnlock() }

	btn := widget.NewButtonWithIcon(action, theme.ConfirmIcon(), doUnlock)

	inputRow := container.NewBorder(nil, nil, nil, btn, pwd)
	box := container.NewVBox(
		widget.NewLabelWithStyle(title, fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		inputRow,
	)
	card := container.NewGridWrap(fyne.NewSize(420, 96), box)
	w.SetContent(container.NewCenter(card))
}

// 对象列表右侧固定列的宽度。
const (
	colSizeW = 100
	colTypeW = 70
	colModW  = 150
	colH     = 30
)

// newMetaCols 构建固定宽度的三列（大小/类型/修改时间），行与列头共用以保证对齐。
func newMetaCols(meta []string, bold bool) fyne.CanvasObject {
	if len(meta) < 3 {
		return nil
	}
	mk := func(text string, wdt float32, align fyne.TextAlign) fyne.CanvasObject {
		lbl := widget.NewLabelWithStyle(text, align, fyne.TextStyle{Bold: bold, Italic: !bold})
		return container.NewGridWrap(fyne.NewSize(wdt, colH), lbl)
	}
	return container.NewHBox(
		mk(meta[0], colSizeW, fyne.TextAlignTrailing),
		mk(meta[1], colTypeW, fyne.TextAlignCenter),
		mk(meta[2], colModW, fyne.TextAlignTrailing),
	)
}

// tappableRow 是一个支持单击选中、双击打开、右键的列表行，可选右侧固定元数据列。
type tappableRow struct {
	widget.BaseWidget
	text         string
	meta         []string
	icon         fyne.Resource
	selected     bool
	onTapped     func()
	onDoubleTap  func()
	secondaryTap func()
}

func (r *tappableRow) withSecondary(fn func()) *tappableRow {
	r.secondaryTap = fn
	return r
}

func (r *tappableRow) TappedSecondary(_ *fyne.PointEvent) {
	if r.secondaryTap != nil {
		r.secondaryTap()
	}
}

func newTappableRow(text string, icon fyne.Resource, onTapped, onDoubleTap func()) *tappableRow {
	r := &tappableRow{text: text, icon: icon, onTapped: onTapped, onDoubleTap: onDoubleTap}
	r.ExtendBaseWidget(r)
	return r
}

func newTappableRowDetail(text string, meta []string, icon fyne.Resource, onTapped, onDoubleTap func()) *tappableRow {
	r := &tappableRow{text: text, meta: meta, icon: icon, onTapped: onTapped, onDoubleTap: onDoubleTap}
	r.ExtendBaseWidget(r)
	return r
}

func (r *tappableRow) Tapped(_ *fyne.PointEvent) {
	if r.onTapped != nil {
		r.onTapped()
	}
}

func (r *tappableRow) DoubleTapped(_ *fyne.PointEvent) {
	if r.onDoubleTap != nil {
		r.onDoubleTap()
	}
}

func (r *tappableRow) setSelected(sel bool) {
	r.selected = sel
	r.Refresh()
}

func (r *tappableRow) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(theme.Color(theme.ColorNameSelection))
	bg.CornerRadius = 4
	bg.Hidden = !r.selected
	icon := widget.NewIcon(r.icon)
	label := widget.NewLabel(r.text)
	left := container.NewHBox(icon, label)
	var content *fyne.Container
	if cols := newMetaCols(r.meta, false); cols != nil {
		content = container.NewBorder(nil, nil, left, cols)
	} else {
		content = container.NewBorder(nil, nil, left, nil)
	}
	return &tappableRowRenderer{row: r, bg: bg, icon: icon, label: label,
		objects: []fyne.CanvasObject{bg, content}, content: content}
}

type tappableRowRenderer struct {
	row     *tappableRow
	bg      *canvas.Rectangle
	icon    *widget.Icon
	label   *widget.Label
	content *fyne.Container
	objects []fyne.CanvasObject
}

func (rr *tappableRowRenderer) Destroy() {}

func (rr *tappableRowRenderer) Layout(size fyne.Size) {
	rr.bg.Resize(size)
	rr.content.Resize(size)
}

func (rr *tappableRowRenderer) MinSize() fyne.Size {
	return rr.content.MinSize()
}

func (rr *tappableRowRenderer) Objects() []fyne.CanvasObject {
	return rr.objects
}

func (rr *tappableRowRenderer) Refresh() {
	rr.bg.Hidden = !rr.row.selected
	rr.bg.FillColor = theme.Color(theme.ColorNameSelection)
	rr.icon.SetResource(rr.row.icon)
	rr.label.SetText(rr.row.text)
	rr.bg.Refresh()
}

// showProperties 弹出一个属性对话框，每个值放在可选中复制（Ctrl+C）的只读输入框里。
func showProperties(w fyne.Window, title string, props [][2]string) {
	items := make([]*widget.FormItem, 0, len(props))
	for _, p := range props {
		entry := widget.NewEntry()
		entry.SetText(p[1])
		items = append(items, widget.NewFormItem(p[0], entry))
	}
	form := widget.NewForm(items...)
	content := container.NewVScroll(form)
	content.SetMinSize(fyne.NewSize(460, 200))
	dialog.ShowCustom(title, "关闭", content, w)
}

// showMain 显示账号管理主界面：左侧账号，右侧 Bucket/对象浏览。
func showMain(w fyne.Window, s *store.Store) {
	var accounts []model.Account
	var rows []*tappableRow
	selected := -1

	accountList := container.NewVBox()
	accountScroll := container.NewVScroll(accountList)

	rightPanel := container.NewStack(widget.NewLabel("双击左侧账号查看其 Bucket"))

	setRight := func(obj fyne.CanvasObject) {
		rightPanel.Objects = []fyne.CanvasObject{obj}
		rightPanel.Refresh()
	}

	selectRow := func(idx int) {
		for i, r := range rows {
			r.setSelected(i == idx)
		}
		selected = idx
	}

	var reload func()
	reload = func() {
		var err error
		accounts, err = s.ListAccounts()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		rows = nil
		accountList.Objects = nil
		for i := range accounts {
			idx := i
			acc := accounts[i]
			row := newTappableRow(acc.Name, theme.AccountIcon(),
				func() { selectRow(idx) },
				func() { selectRow(idx); showBuckets(w, setRight, &acc) },
			)
			rows = append(rows, row)
			accountList.Add(row)
		}
		selected = -1
		accountList.Refresh()
	}
	reload()

	addBtn := widget.NewButtonWithIcon("新增", theme.ContentAddIcon(), func() {
		editAccount(w, s, nil, reload)
	})
	editBtn := widget.NewButtonWithIcon("编辑", theme.DocumentCreateIcon(), func() {
		if selected < 0 || selected >= len(accounts) {
			dialog.ShowInformation("提示", "请先选择一个账号", w)
			return
		}
		acc := accounts[selected]
		editAccount(w, s, &acc, reload)
	})
	delBtn := widget.NewButtonWithIcon("删除", theme.DeleteIcon(), func() {
		if selected < 0 || selected >= len(accounts) {
			dialog.ShowInformation("提示", "请先选择一个账号", w)
			return
		}
		acc := accounts[selected]
		dialog.ShowConfirm("确认删除", fmt.Sprintf("确定删除账号 %q 吗？", acc.Name), func(ok bool) {
			if !ok {
				return
			}
			if err := s.DeleteAccount(acc.ID); err != nil {
				dialog.ShowError(err, w)
				return
			}
			reload()
		}, w)
	})
	testBtn := widget.NewButton("测试连接", func() {
		if selected < 0 || selected >= len(accounts) {
			dialog.ShowInformation("提示", "请先选择一个账号", w)
			return
		}
		acc := accounts[selected]
		prog := dialog.NewCustomWithoutButtons("测试中", widget.NewLabel("正在连接..."), w)
		prog.Show()
		go func() {
			buckets, err := awss3.ListBuckets(&acc)
			fyne.Do(func() {
				prog.Hide()
				if err != nil {
					dialog.ShowError(fmt.Errorf("连接失败: %w", err), w)
					return
				}
				dialog.ShowInformation("连接成功", fmt.Sprintf("发现 %d 个桶", len(buckets)), w)
			})
		}()
	})

	toolbar := container.NewHBox(addBtn, editBtn, delBtn, testBtn)
	leftPanel := container.NewBorder(toolbar, nil, nil, nil, accountScroll)

	lockItem := fyne.NewMenuItem("加锁", func() {
		s.Close()
		showUnlock(w)
	})
	locItem := fyne.NewMenuItem("修改存储位置...", func() {
		cur, _ := store.CurrentDBPath()
		go func() {
			newPath, err := zenity.SelectFileSave(
				zenity.Title("选择新的数据库文件位置"),
				zenity.Filename(cur),
				zenity.ConfirmOverwrite(),
			)
			if err != nil {
				return
			}
			fyne.Do(func() {
				dialog.ShowConfirm("修改存储位置",
					fmt.Sprintf("将把数据库移动到:\n%s\n\n之后需要重新输入主密码解锁。是否继续？", newPath),
					func(ok bool) {
						if !ok {
							return
						}
						s.Close()
						if e := store.ChangeLocation(newPath); e != nil {
							dialog.ShowError(e, w)
						}
						showUnlock(w)
					}, w)
			})
		}()
	})
	menu := fyne.NewMenu("菜单", lockItem, locItem)
	w.SetMainMenu(fyne.NewMainMenu(menu))

	split := container.NewHSplit(leftPanel, rightPanel)
	split.Offset = 0.3
	w.SetContent(split)
}

// showBuckets 在右侧面板显示某账号下的 Bucket 列表。
func showBuckets(w fyne.Window, setRight func(fyne.CanvasObject), acc *model.Account) {
	loading := widget.NewLabel(fmt.Sprintf("正在加载 %s 的 Bucket...", acc.Name))
	setRight(loading)

	go func() {
		infos, err := awss3.ListBucketsWithAccess(acc)
		fyne.Do(func() {
			if err != nil {
				setRight(widget.NewLabel(fmt.Sprintf("加载失败: %v", err)))
				return
			}
			list := container.NewVBox()
			for _, bi := range infos {
				b := bi
				status := "✓ 可访问"
				if !b.Accessible {
					status = "✗ 无权限"
				}
				accessPlain := "可访问"
				if !b.Accessible {
					accessPlain = "无权限"
				}
				row := newTappableRowDetail(b.Name, []string{"", "", status}, theme.StorageIcon(), nil,
					func() { showObjects(w, setRight, acc, b.Name, "") },
				).withSecondary(func() {
					showProperties(w, "Bucket 属性", [][2]string{
						{"名称", b.Name},
						{"访问权限", accessPlain},
					})
				})
				list.Add(row)
			}
			title := widget.NewLabelWithStyle(
				fmt.Sprintf("账号 %s — 共 %d 个 Bucket（双击进入）", acc.Name, len(infos)),
				fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			nameHeader := widget.NewLabelWithStyle("名称", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			permHeader := newMetaCols([]string{"", "", "权限"}, true)
			header := container.NewBorder(nil, nil, nameHeader, permHeader)
			top := container.NewVBox(title, header)
			setRight(container.NewBorder(top, nil, nil, nil, container.NewVScroll(list)))
		})
	}()
}

// showObjects 加载某 Bucket 内 prefix 层级的对象，然后构建带底部操作栏的视图。
func showObjects(w fyne.Window, setRight func(fyne.CanvasObject), acc *model.Account, bucket, prefix string) {
	loading := widget.NewLabel(fmt.Sprintf("正在加载 %s/%s ...", bucket, prefix))
	setRight(loading)

	go func() {
		objects, err := awss3.ListObjects(acc, bucket, prefix)
		fyne.Do(func() {
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			buildObjectView(w, setRight, acc, bucket, prefix, objects, errMsg)
		})
	}()
}

// buildBreadcrumb 构建可点击的面包屑路径栏：bucket / seg1 / seg2 / ...
func buildBreadcrumb(w fyne.Window, setRight func(fyne.CanvasObject), acc *model.Account, bucket, prefix string) fyne.CanvasObject {
	crumbs := container.NewHBox()
	bucketLink := widget.NewHyperlink(bucket, nil)
	bucketLink.OnTapped = func() { showObjects(w, setRight, acc, bucket, "") }
	crumbs.Add(bucketLink)

	if prefix != "" {
		parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
		for i, part := range parts {
			crumbs.Add(widget.NewLabel("/"))
			seg := strings.Join(parts[:i+1], "/") + "/"
			link := widget.NewHyperlink(part, nil)
			link.OnTapped = func() { showObjects(w, setRight, acc, bucket, seg) }
			crumbs.Add(link)
		}
	}
	return crumbs
}

// buildObjectView 构建对象浏览器：面包屑 + 过滤框 + 文件/文件夹列表 + 底部操作栏 + 进度条。
func buildObjectView(w fyne.Window, setRight func(fyne.CanvasObject), acc *model.Account, bucket, prefix string, objects []awss3.Object, errMsg string) {
	var rows []*tappableRow
	var selectedObj *awss3.Object

	selectRow := func(idx int, obj *awss3.Object) {
		for i, r := range rows {
			r.setSelected(i == idx)
		}
		selectedObj = obj
	}

	refresh := func() { showObjects(w, setRight, acc, bucket, prefix) }

	list := container.NewVBox()

	buildList := func(filter string) {
		rows = nil
		list.Objects = nil
		selectedObj = nil

		backBtn := newTappableRow("⬆ 返回上一级", theme.NavigateBackIcon(), nil, func() {
			if prefix == "" {
				showBuckets(w, setRight, acc)
				return
			}
			showObjects(w, setRight, acc, bucket, parentPrefix(prefix))
		})
		rows = append(rows, backBtn)
		list.Add(backBtn)

		lf := strings.ToLower(filter)
		for _, o := range objects {
			obj := o
			if lf != "" && !strings.Contains(strings.ToLower(obj.Name), lf) {
				continue
			}
			idx := len(rows)
			if obj.IsDir {
				row := newTappableRowDetail(obj.Name, []string{"", "文件夹", ""}, theme.FolderIcon(),
					func() { selectRow(idx, &obj) },
					func() { showObjects(w, setRight, acc, bucket, obj.Key) },
				).withSecondary(func() {
					showProperties(w, "文件夹属性", [][2]string{
						{"名称", obj.Name},
						{"Key", obj.Key},
						{"类型", "文件夹"},
					})
				})
				rows = append(rows, row)
				list.Add(row)
			} else {
				meta := []string{
					humanSize(obj.Size),
					obj.Type,
					obj.LastModified.Local().Format("2006-01-02 15:04"),
				}
				row := newTappableRowDetail(obj.Name, meta, theme.FileIcon(),
					func() { selectRow(idx, &obj) }, nil).withSecondary(func() {
					showProperties(w, "文件属性", [][2]string{
						{"名称", obj.Name},
						{"Key", obj.Key},
						{"大小", humanSize(obj.Size)},
						{"类型", obj.Type},
						{"最后修改", obj.LastModified.Local().Format("2006-01-02 15:04:05")},
					})
				})
				rows = append(rows, row)
				list.Add(row)
			}
		}
		list.Refresh()
	}
	buildList("")

	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("输入关键字过滤...")
	filterEntry.OnChanged = func(s string) { buildList(s) }

	nameHeader := widget.NewLabelWithStyle("名称", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	metaHeader := newMetaCols([]string{"大小", "类型", "修改时间"}, true)
	header := container.NewBorder(nil, nil, nameHeader, metaHeader)

	progress := widget.NewProgressBar()

	refreshBtn := widget.NewButtonWithIcon("刷新", theme.ViewRefreshIcon(), refresh)

	uploadBtn := widget.NewButtonWithIcon("上传文件", theme.UploadIcon(), func() {
		go func() {
			localPath, err := zenity.SelectFile(zenity.Title("选择要上传的文件"))
			if err != nil {
				return
			}
			key := prefix + filepath.Base(localPath)
			e := awss3.UploadFile(acc, bucket, key, localPath, func(done, total int64) {
				fyne.Do(func() {
					if total > 0 {
						progress.SetValue(float64(done) / float64(total))
					}
				})
			})
			fyne.Do(func() {
				progress.SetValue(0)
				if e != nil {
					dialog.ShowError(e, w)
					return
				}
				dialog.ShowInformation("完成", "上传成功", w)
				refresh()
			})
		}()
	})

	downloadBtn := widget.NewButtonWithIcon("下载文件", theme.DownloadIcon(), func() {
		if selectedObj == nil || selectedObj.IsDir {
			dialog.ShowInformation("提示", "请先选择一个文件", w)
			return
		}
		obj := *selectedObj
		go func() {
			savePath, err := zenity.SelectFileSave(
				zenity.Title("保存到"),
				zenity.Filename(obj.Name),
				zenity.ConfirmOverwrite(),
			)
			if err != nil {
				return
			}
			e := awss3.DownloadFile(acc, bucket, obj.Key, savePath, func(done, total int64) {
				fyne.Do(func() {
					if total > 0 {
						progress.SetValue(float64(done) / float64(total))
					}
				})
			})
			fyne.Do(func() {
				progress.SetValue(0)
				if e != nil {
					dialog.ShowError(e, w)
					return
				}
				revealInExplorer(savePath)
				dialog.ShowInformation("完成", "下载成功", w)
			})
		}()
	})

	deleteBtn := widget.NewButtonWithIcon("删除文件", theme.DeleteIcon(), func() {
		if selectedObj == nil {
			dialog.ShowInformation("提示", "请先选择一个对象", w)
			return
		}
		obj := *selectedObj
		kind := "文件"
		if obj.IsDir {
			kind = "文件夹（仅删除占位对象，不递归删除内容）"
		}
		dialog.ShowConfirm("确认删除", fmt.Sprintf("确定删除%s %q 吗？", kind, obj.Name), func(ok bool) {
			if !ok {
				return
			}
			dialog.ShowConfirm("再次确认", fmt.Sprintf("此操作不可撤销，确定要删除 %q 吗？", obj.Name), func(ok2 bool) {
				if !ok2 {
					return
				}
				go func() {
					e := awss3.DeleteObject(acc, bucket, obj.Key)
					fyne.Do(func() {
						if e != nil {
							dialog.ShowError(e, w)
							return
						}
						refresh()
					})
				}()
			}, w)
		}, w)
	})

	urlBtn := widget.NewButtonWithIcon("Web URL", theme.ContentCopyIcon(), func() {
		if selectedObj == nil || selectedObj.IsDir {
			dialog.ShowInformation("提示", "请先选择一个文件", w)
			return
		}
		obj := *selectedObj

		minutesEntry := widget.NewEntry()
		minutesEntry.SetText("10")
		urlEntry := widget.NewMultiLineEntry()
		urlEntry.Wrapping = fyne.TextWrapBreak
		urlEntry.SetPlaceHolder("生成中...")

		generateURL := func(minutes string) {
			m := 10
			if v, err := fmt.Sscanf(minutes, "%d", &m); err != nil || v != 1 || m <= 0 {
				m = 10
			}
			go func() {
				url, e := awss3.PresignURL(acc, bucket, obj.Key, time.Duration(m)*time.Minute)
				fyne.Do(func() {
					if e != nil {
						urlEntry.SetText("生成失败: " + e.Error())
						return
					}
					urlEntry.SetText(url)
				})
			}()
		}
		generateURL(minutesEntry.Text)
		minutesEntry.OnChanged = generateURL

		urlScroll := container.NewVScroll(urlEntry)
		urlScroll.SetMinSize(fyne.NewSize(620, 220))
		form := container.NewBorder(
			container.NewBorder(nil, nil, widget.NewLabel("过期时间(分钟):"), nil, minutesEntry),
			nil, nil, nil,
			urlScroll,
		)
		dialog.ShowCustom("预签名 URL", "关闭", form, w)
	})

	mkdirBtn := widget.NewButtonWithIcon("新建文件夹", theme.FolderNewIcon(), func() {
		entry := widget.NewEntry()
		entry.SetPlaceHolder("文件夹名")
		dialog.ShowForm("新建文件夹", "创建", "取消",
			[]*widget.FormItem{widget.NewFormItem("名称", entry)}, func(ok bool) {
				if !ok || entry.Text == "" {
					return
				}
				go func() {
					e := awss3.CreateFolder(acc, bucket, prefix, entry.Text)
					fyne.Do(func() {
						if e != nil {
							dialog.ShowError(e, w)
							return
						}
						refresh()
					})
				}()
			}, w)
	})

	actionBar := container.NewHBox(refreshBtn, uploadBtn, downloadBtn, deleteBtn, urlBtn, mkdirBtn)

	msgLabel := widget.NewLabel("")
	msgLabel.Wrapping = fyne.TextWrapWord
	if errMsg != "" {
		msgLabel.SetText("⚠ 加载失败: " + errMsg)
	}
	msgScroll := container.NewVScroll(msgLabel)
	msgScroll.SetMinSize(fyne.NewSize(0, 60))
	bottom := container.NewVBox(actionBar, progress, msgScroll)

	breadcrumb := buildBreadcrumb(w, setRight, acc, bucket, prefix)
	top := container.NewVBox(breadcrumb, filterEntry, header)
	setRight(container.NewBorder(top, bottom, nil, nil, container.NewVScroll(list)))
}

// parentPrefix 返回上一级前缀，如 "a/b/c/" -> "a/b/"。
func parentPrefix(prefix string) string {
	trimmed := prefix
	if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '/' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	for i := len(trimmed) - 1; i >= 0; i-- {
		if trimmed[i] == '/' {
			return trimmed[:i+1]
		}
	}
	return ""
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// editAccount 弹出账号编辑表单；acc 为 nil 表示新增。
func editAccount(w fyne.Window, s *store.Store, acc *model.Account, onDone func()) {
	name := widget.NewEntry()
	endpoint := widget.NewEntry()
	endpoint.SetPlaceHolder("留空则使用 AWS 默认端点")
	region := widget.NewEntry()
	region.SetPlaceHolder("如 us-east-1")
	accessKey := widget.NewEntry()
	secretKey := widget.NewPasswordEntry()
	pathStyle := widget.NewCheck("使用 Path-Style (兼容 MinIO 等)", nil)

	isEdit := acc != nil
	if isEdit {
		name.SetText(acc.Name)
		endpoint.SetText(acc.Endpoint)
		region.SetText(acc.Region)
		accessKey.SetText(acc.AccessKeyID)
		secretKey.SetText(acc.SecretAccessKey)
		pathStyle.SetChecked(acc.UsePathStyle)
	}

	items := []*widget.FormItem{
		widget.NewFormItem("名称", name),
		widget.NewFormItem("Endpoint", endpoint),
		widget.NewFormItem("Region", region),
		widget.NewFormItem("Access Key ID", accessKey),
		widget.NewFormItem("Secret Access Key", secretKey),
		widget.NewFormItem("", pathStyle),
	}

	title := "新增账号"
	if isEdit {
		title = "编辑账号"
	}

	dialog.ShowForm(title, "保存", "取消", items, func(ok bool) {
		if !ok {
			return
		}
		if name.Text == "" || accessKey.Text == "" {
			dialog.ShowInformation("提示", "名称和 Access Key ID 不能为空", w)
			return
		}
		a := &model.Account{
			Name:            name.Text,
			Endpoint:        endpoint.Text,
			Region:          region.Text,
			AccessKeyID:     accessKey.Text,
			SecretAccessKey: secretKey.Text,
			UsePathStyle:    pathStyle.Checked,
		}
		var err error
		if isEdit {
			a.ID = acc.ID
			err = s.UpdateAccount(a)
		} else {
			err = s.AddAccount(a)
		}
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		onDone()
	}, w)
}

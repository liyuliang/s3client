package ui

import (
	_ "embed"
	"crypto/rand"
	"errors"
	"fmt"
	"image/color"
	"context"
	mrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	"s3client/internal/share"
	"s3client/internal/sharepoint"
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

func (appTheme) Font(s fyne.TextStyle) fyne.Resource     { return theme.DefaultTheme().Font(s) }
func (appTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }
func (appTheme) Size(n fyne.ThemeSizeName) float32       { return theme.DefaultTheme().Size(n) }

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
			showInfo("提示", "主密码不能为空", w)
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
		showMain(w, s, nil)
	}
	pwd.OnSubmitted = func(string) { doUnlock() }

	btn := widget.NewButtonWithIcon(action, theme.ConfirmIcon(), doUnlock)

	// 不需要账号库也能查看别人分享的 bucket（只读）
	shareBtn := widget.NewButtonWithIcon("加载分享配置", theme.MailAttachmentIcon(), func() {
		loadShare(w, func(sess *share.Session) { showMain(w, nil, sess) })
	})

	gap := canvas.NewRectangle(color.Transparent)
	gap.SetMinSize(fyne.NewSize(0, 10))

	inputRow := container.NewBorder(nil, nil, nil, btn, pwd)
	box := container.NewVBox(
		widget.NewLabelWithStyle(title, fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		inputRow,
		gap,
		shareBtn,
	)
	card := container.NewGridWrap(fyne.NewSize(420, 140), box)
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
	nameColor    color.Color
	check         *widget.Check
	onTapped      func()
	onDoubleTap   func()
	secondaryTap  func()
	secondaryMenu []*fyne.MenuItem
	lastTap       time.Time
}

// rowDoubleTapDelay 本行自己判定双击的间隔。
// 行不实现 fyne.DoubleTappable：否则 Fyne 会让单击进入双击等待，
// 连续点击多个文件时前一次的单击会被取消，导致勾选完全不生效。
const rowDoubleTapDelay = 400 * time.Millisecond

func (r *tappableRow) withSecondary(fn func()) *tappableRow {
	r.secondaryTap = fn
	return r
}

// withSecondaryMenu 给行加一个右键上下文菜单，右键时在点击位置弹出。
func (r *tappableRow) withSecondaryMenu(items ...*fyne.MenuItem) *tappableRow {
	r.secondaryMenu = items
	return r
}

func (r *tappableRow) withColor(c color.Color) *tappableRow {
	r.nameColor = c
	return r
}

// withCheck 给行左侧加一个复选框（用于多选下载），勾选状态变化时回调。
// 加了复选框后，单击行（除复选框本身以外的位置）会自动勾上；点复选框本身可切换。
func (r *tappableRow) withCheck(onChanged func(bool)) *tappableRow {
	r.check = widget.NewCheck("", onChanged)
	return r
}

func (r *tappableRow) TappedSecondary(e *fyne.PointEvent) {
	if len(r.secondaryMenu) > 0 {
		if c := fyne.CurrentApp().Driver().CanvasForObject(r); c != nil {
			widget.ShowPopUpMenuAtPosition(fyne.NewMenu("", r.secondaryMenu...), c, e.AbsolutePosition)
		}
		return
	}
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

// Tapped 处理左键单击：行有双击动作时，短时间内第二次单击视为双击；
// 带复选框的行单击即勾上（取消勾选请点复选框本身）。
func (r *tappableRow) Tapped(_ *fyne.PointEvent) {
	now := time.Now()
	if r.onDoubleTap != nil && !r.lastTap.IsZero() && now.Sub(r.lastTap) <= rowDoubleTapDelay {
		r.lastTap = time.Time{}
		r.onDoubleTap()
		return
	}
	r.lastTap = now
	if r.onTapped != nil {
		r.onTapped()
	}
	if r.check != nil {
		r.check.SetChecked(true)
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

	var nameObj fyne.CanvasObject
	var setText func(string)
	if r.nameColor != nil {
		t := canvas.NewText(r.text, r.nameColor)
		t.TextSize = theme.TextSize()
		nameObj = container.NewCenter(t)
		setText = func(s string) { t.Text = s; t.Refresh() }
	} else {
		l := widget.NewLabel(r.text)
		nameObj = l
		setText = func(s string) { l.SetText(s) }
	}

	var left *fyne.Container
	if r.check != nil {
		left = container.NewHBox(r.check, icon, nameObj)
	} else {
		left = container.NewHBox(icon, nameObj)
	}
	var content *fyne.Container
	if cols := newMetaCols(r.meta, false); cols != nil {
		content = container.NewBorder(nil, nil, left, cols)
	} else {
		content = container.NewBorder(nil, nil, left, nil)
	}
	return &tappableRowRenderer{row: r, bg: bg, icon: icon, setText: setText,
		objects: []fyne.CanvasObject{bg, content}, content: content}
}

type tappableRowRenderer struct {
	row     *tappableRow
	bg      *canvas.Rectangle
	icon    *widget.Icon
	setText func(string)
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
	rr.setText(rr.row.text)
	rr.bg.Refresh()
}

// askPassword 弹出一个输入主密码的对话框（约 360px 宽），确定且非空时回调。
func askPassword(w fyne.Window, title string, onOK func(pw string)) {
	pw := widget.NewPasswordEntry()
	pw.SetPlaceHolder("主密码")
	content := container.NewVScroll(pw)
	content.SetMinSize(fyne.NewSize(360, 60))
	dialog.ShowCustomConfirm(title, "确定", "取消", content, func(ok bool) {
		if !ok || pw.Text == "" {
			return
		}
		onOK(pw.Text)
	}, w)
}

// showInfo 显示一个约 300px 宽的信息对话框（参数顺序与 dialog.ShowInformation 一致）。
func showInfo(title, message string, w fyne.Window) {
	lbl := widget.NewLabel(message)
	lbl.Wrapping = fyne.TextWrapWord
	content := container.NewVScroll(lbl)
	content.SetMinSize(fyne.NewSize(300, 60))
	dialog.ShowCustom(title, "确定", content, w)
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

// loadShare 选择一个分享配置文件并用口令解密，成功后回调只读会话。
func loadShare(w fyne.Window, onLoaded func(*share.Session)) {
	go func() {
		path, zerr := zenity.SelectFile(zenity.Title("选择分享配置文件（.s3share）"))
		if zerr != nil {
			return
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			fyne.Do(func() { dialog.ShowError(rerr, w) })
			return
		}
		fyne.Do(func() { openShareConfig(w, data, onLoaded) })
	}()
}

// openShareConfig 打开分享配置：免口令文件直接打开，需要口令时才弹输入框。
func openShareConfig(w fyne.Window, data []byte, onOK func(*share.Session)) {
	sess, err := share.Open(data, "")
	if errors.Is(err, share.ErrPasswordRequired) {
		askSharePassword(w, data, onOK)
		return
	}
	if err != nil {
		dialog.ShowError(err, w)
		return
	}
	if sess.Expired() {
		showInfo("分享已过期",
			fmt.Sprintf("该分享的有效期已于 %s 结束，无法使用。", sess.ExpiresAt.Local().Format("2006-01-02 15:04")), w)
		return
	}
	onOK(sess)
}

// askSharePassword 弹出输入分享口令的对话框，解密失败/已过期都会提示。
func askSharePassword(w fyne.Window, data []byte, onOK func(*share.Session)) {
	pw := widget.NewPasswordEntry()
	pw.SetPlaceHolder("分享口令")
	content := container.NewVScroll(pw)
	content.SetMinSize(fyne.NewSize(360, 60))
	dialog.ShowCustomConfirm("加载分享配置", "确定", "取消", content, func(ok bool) {
		if !ok || pw.Text == "" {
			return
		}
		sess, err := share.Open(data, pw.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		if sess.Expired() {
			showInfo("分享已过期",
				fmt.Sprintf("该分享的有效期已于 %s 结束，无法使用。", sess.ExpiresAt.Local().Format("2006-01-02 15:04")), w)
			return
		}
		onOK(sess)
	}, w)
}

// checkShareValid 只读分享过期时提示并返回 false。
func checkShareValid(w fyne.Window, acc *model.Account) bool {
	if acc == nil || !acc.ShareExpired() {
		return true
	}
	showInfo("分享已过期",
		fmt.Sprintf("该分享的有效期已于 %s 结束，无法继续使用。", acc.ShareExpiresAt.Local().Format("2006-01-02 15:04")), w)
	return false
}

// openShare 双击分享条目：校验有效期后直接进入该 bucket（只读）。
func openShare(w fyne.Window, setRight func(fyne.CanvasObject), sess *share.Session) {
	if sess.Expired() {
		showInfo("分享已过期",
			fmt.Sprintf("该分享的有效期已于 %s 结束，无法使用。", sess.ExpiresAt.Local().Format("2006-01-02 15:04")), w)
		return
	}
	showObjects(w, setRight, nil, sess.Account, sess.Bucket, "")
}

// showShareInfo 显示分享配置的基本信息（不含任何凭据）。
func showShareInfo(w fyne.Window, sess *share.Session) {
	exp := "不限期"
	if !sess.ExpiresAt.IsZero() {
		exp = sess.ExpiresAt.Local().Format("2006-01-02 15:04")
	}
	showProperties(w, "分享信息", [][2]string{
		{"名称", sess.Name},
		{"Bucket", sess.Bucket},
		{"权限", "只读（禁止上传/删除/新建文件夹）"},
		{"有效期至", exp},
		{"状态", sess.Remaining()},
	})
}

// shareCodeLen 分享口令长度。
const shareCodeLen = 8

// randomShareCode 生成 8 位随机分享口令（去掉 0/O/1/l/I 等易混淆字符）。
func randomShareCode() string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	buf := make([]byte, shareCodeLen)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(mrand.IntN(256))
		}
	}
	for i := range buf {
		buf[i] = chars[int(buf[i])%len(chars)]
	}
	return string(buf)
}

// showCreateShare 生成当前 bucket 的只读分享配置并保存到文件。
func showCreateShare(w fyne.Window, acc *model.Account, bucket string) {
	nameEntry := widget.NewEntry()
	nameEntry.SetText(bucket)
	hoursEntry := widget.NewEntry()
	hoursEntry.SetText("24")
	pw := widget.NewEntry()
	pw.SetText(randomShareCode())
	form := widget.NewForm(
		widget.NewFormItem("分享名称", nameEntry),
		widget.NewFormItem("有效期(小时, 0=不限)", hoursEntry),
		widget.NewFormItem("分享口令(自动生成, 可改, 留空=免口令)", pw),
	)
	// 实时回显有效期，便于确认输入是否生效
	hint := widget.NewLabel("")
	updateHint := func(text string) {
		h := 0
		if v, err := fmt.Sscanf(text, "%d", &h); err != nil || v != 1 || h < 0 {
			h = 0
		}
		if h == 0 {
			hint.SetText("有效期至：不限期")
			return
		}
		hint.SetText("有效期至：" + time.Now().Add(time.Duration(h)*time.Hour).Local().Format("2006-01-02 15:04"))
	}
	hoursEntry.OnChanged = updateHint
	updateHint(hoursEntry.Text)
	scroll := container.NewVScroll(form)
	scroll.SetMinSize(fyne.NewSize(420, 260))
	dialog.ShowCustomConfirm("生成只读分享配置", "生成并保存", "取消",
		container.NewVBox(scroll, hint), func(ok bool) {
		if !ok {
			return
		}
		hours := 0
		if v, err := fmt.Sscanf(hoursEntry.Text, "%d", &hours); err != nil || v != 1 || hours < 0 {
			hours = 0
		}
		ttl := time.Duration(hours) * time.Hour
		data, err := share.Create(nameEntry.Text, bucket, acc, ttl, pw.Text)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		expText := "不限期（0 小时）"
		if ttl > 0 {
			expText = fmt.Sprintf("%d 小时（至 %s）", hours, time.Now().Add(ttl).Local().Format("2006-01-02 15:04"))
		}
		// 留空口令 = 免口令：随机口令随文件保存，对方拿到文件即可打开
		pwText := "免口令（口令随文件保存，对方无需输入）"
		if pw.Text != "" {
			pwText = "分享口令：" + pw.Text
		}
		go func() {
			savePath, zerr := zenity.SelectFileSave(
				zenity.Title("保存分享配置文件"),
				zenity.Filename(bucket+"-readonly.s3share"),
				zenity.ConfirmOverwrite(),
			)
			if zerr != nil {
				return
			}
			werr := os.WriteFile(savePath, data, 0600)
			fyne.Do(func() {
				if werr != nil {
					dialog.ShowError(werr, w)
					return
				}
				showInfo("完成", fmt.Sprintf(
					"已生成只读分享配置：\n%s\n\n有效期：%s\n\n%s",
					savePath, expText, pwText), w)
			})
		}()
	}, w)
}

// accountLabel 返回带类型标记的账号显示名：S3 账号标「S3」，SharePoint 账号标「SP」。
func accountLabel(name string, isSharePoint bool) string {
	if isSharePoint {
		return name + "（SP）"
	}
	return name + "（S3）"
}

// showMain 显示账号管理主界面：左侧账号，右侧 Bucket/对象浏览。
// s 为 nil 表示未解锁账号库（只用分享配置浏览），initialShare 非 nil 时会自动打开该分享。
func showMain(w fyne.Window, s *store.Store, initialShare *share.Session) {
	var accounts []model.Account
	var rows []*tappableRow
	selected := -1

	accountList := container.NewVBox()
	accountScroll := container.NewVScroll(accountList)

	// 右侧：每个账号一个 tab，切换 tab 不重新加载。
	// tab 的内容是一个 Stack，账号内导航（bucket → 目录）只替换该 tab 内部的内容。
	rightPanel := container.NewStack(widget.NewLabel("双击左侧账号查看其 Bucket"))
	tabs := container.NewAppTabs()
	tabsShown := false
	showTabs := func() {
		if tabsShown {
			return
		}
		tabsShown = true
		rightPanel.Objects = []fyne.CanvasObject{tabs}
		rightPanel.Refresh()
	}

	type tabHandle struct {
		item  *container.TabItem
		panel *fyne.Container
	}
	tabByKey := map[string]*tabHandle{}

	// openTab 打开（或切换到已存在的）tab；已存在时只切换、不重新加载。
	openTab := func(key, title string, open func(setRight func(fyne.CanvasObject))) {
		if h, ok := tabByKey[key]; ok {
			tabs.Select(h.item)
			return
		}
		panel := container.NewStack(widget.NewLabel("加载中..."))
		item := container.NewTabItem(title, panel)
		h := &tabHandle{item: item, panel: panel}
		tabByKey[key] = h
		tabs.Append(item)
		tabs.Select(item)
		showTabs()
		open(func(obj fyne.CanvasObject) {
			panel.Objects = []fyne.CanvasObject{obj}
			panel.Refresh()
		})
	}

	// closeTabsNotIn 删除账号/分享已不存在的 tab，并同步 tab 标题。
	syncTabs := func(keep map[string]string) {
		if len(tabByKey) == 0 {
			return
		}
		var items []*container.TabItem
		for key, h := range tabByKey {
			if title, ok := keep[key]; ok {
				h.item.Text = title
				items = append(items, h.item)
				continue
			}
			delete(tabByKey, key)
		}
		tabs.SetItems(items)
		tabs.Refresh()
	}

	selectRow := func(idx int) {
		for i, r := range rows {
			r.setSelected(i == idx)
		}
		selected = idx
	}

	// 分享会话是只存在于内存中的只读条目，不写入账号库。
	var sessions []*share.Session
	if initialShare != nil {
		sessions = append(sessions, initialShare)
	}

	// 选中分享行时清空账号选中项，避免菜单栏的账号操作误用。
	selectShareRow := func(idx int) {
		for i, r := range rows {
			r.setSelected(i == idx)
		}
		selected = -1
	}

	var reload func()
	var doEdit, doDelete, doTest func(acc model.Account)
	reload = func() {
		if s != nil {
			var err error
			accounts, err = s.ListAccounts()
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
		} else {
			accounts = nil
		}
		rows = nil
		accountList.Objects = nil
		for i := range accounts {
			idx := i
			acc := accounts[i]
			row := newTappableRow(accountLabel(acc.Name, acc.IsSharePoint()), theme.AccountIcon(),
				func() { selectRow(idx) },
				func() {
					selectRow(idx)
					openTab(fmt.Sprintf("acc:%d", acc.ID), accountLabel(acc.Name, acc.IsSharePoint()), func(setRight func(fyne.CanvasObject)) {
						if acc.IsSharePoint() {
							showSharePointDrives(w, setRight, s, &acc)
						} else {
							showBuckets(w, setRight, s, &acc)
						}
					})
				},
			).withSecondaryMenu(
				fyne.NewMenuItem("编辑账号...", func() { selectRow(idx); doEdit(acc) }),
				fyne.NewMenuItem("删除账号", func() { selectRow(idx); doDelete(acc) }),
				fyne.NewMenuItem("测试连接", func() { selectRow(idx); doTest(acc) }),
			)
			rows = append(rows, row)
			accountList.Add(row)
		}
		keep := map[string]string{}
		for _, a := range accounts {
			keep[fmt.Sprintf("acc:%d", a.ID)] = accountLabel(a.Name, a.IsSharePoint())
		}
		for i, sess := range sessions {
			key := fmt.Sprintf("share:%p", sess)
			keep[key] = fmt.Sprintf("🔒 %s（只读）", sess.Name)
			idx := len(accounts) + i
			si := i
			cur := sess
			expText := "不限期"
			if !cur.ExpiresAt.IsZero() {
				expText = "有效期至" + cur.ExpiresAt.Local().Format("2006-01-02 15:04")
			}
			row := newTappableRow(fmt.Sprintf("🔒 %s（只读，%s）", cur.Name, expText), theme.MailAttachmentIcon(),
				func() { selectShareRow(idx) },
				func() {
					openTab(key, fmt.Sprintf("🔒 %s（只读）", cur.Name), func(setRight func(fyne.CanvasObject)) {
						openShare(w, setRight, cur)
					})
				},
			).withSecondaryMenu(
				fyne.NewMenuItem("分享信息...", func() { showShareInfo(w, cur) }),
				fyne.NewMenuItem("移除该分享", func() {
					sessions = append(sessions[:si], sessions[si+1:]...)
					reload()
				}),
			)
			rows = append(rows, row)
			accountList.Add(row)
		}
		syncTabs(keep)
		selected = -1
		accountList.Refresh()
	}
	reload()

	// 从解锁界面加载进来的分享：直接打开它的 tab
	if initialShare != nil {
		first := initialShare
		openTab(fmt.Sprintf("share:%p", first), fmt.Sprintf("🔒 %s（只读）", first.Name), func(setRight func(fyne.CanvasObject)) {
			openShare(w, setRight, first)
		})
	}

	doEdit = func(acc model.Account) {
		editAccount(w, s, &acc, reload)
	}
	doDelete = func(acc model.Account) {
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
	}
	doTest = func(acc model.Account) {
		prog := dialog.NewCustomWithoutButtons("测试中", widget.NewLabel("正在连接..."), w)
		prog.Show()
		go func() {
			if acc.IsSharePoint() {
				cfg := spConfig(&acc)
				err := sharepoint.TestConnection(cfg)
				fyne.Do(func() {
					prog.Hide()
					if err != nil {
						dialog.ShowError(fmt.Errorf("连接失败: %w", err), w)
						return
					}
					showInfo("连接成功", "SharePoint 站点连接正常", w)
				})
			} else {
				buckets, err := awss3.ListBuckets(&acc)
				fyne.Do(func() {
					prog.Hide()
					if err != nil {
						dialog.ShowError(fmt.Errorf("连接失败: %w", err), w)
						return
					}
					showInfo("连接成功", fmt.Sprintf("发现 %d 个桶", len(buckets)), w)
				})
			}
		}()
	}

	panelTitleText := "账号（右键可编辑/删除/测试）"
	if s == nil {
		panelTitleText = "分享（未解锁账号库，右键可查看信息/移除）"
	}
	panelTitle := widget.NewLabelWithStyle(panelTitleText, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	leftPanel := container.NewBorder(panelTitle, nil, nil, nil, accountScroll)

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
	changePwItem := fyne.NewMenuItem("修改主密码...", func() {
		newPw := widget.NewPasswordEntry()
		confirmPw := widget.NewPasswordEntry()
		items := []*widget.FormItem{
			widget.NewFormItem("新主密码", newPw),
			widget.NewFormItem("确认新主密码", confirmPw),
		}
		form := widget.NewForm(items...)
		formScroll := container.NewVScroll(form)
		formScroll.SetMinSize(fyne.NewSize(380, 140))
		dialog.ShowCustomConfirm("修改主密码", "确定", "取消", formScroll, func(ok bool) {
			if !ok {
				return
			}
			if newPw.Text == "" {
				showInfo("提示", "新主密码不能为空", w)
				return
			}
			if newPw.Text != confirmPw.Text {
				showInfo("提示", "两次输入的主密码不一致", w)
				return
			}
			if err := s.ChangeMasterPassword(newPw.Text); err != nil {
				dialog.ShowError(err, w)
				return
			}
			showInfo("完成", "主密码已更新", w)
		}, w)
	})
	exportItem := fyne.NewMenuItem("导出账号...", func() {
		askPassword(w, "导出账号 — 输入主密码", func(pw string) {
			go func() {
				data, err := s.ExportAccounts(pw)
				if err != nil {
					fyne.Do(func() { dialog.ShowError(err, w) })
					return
				}
				savePath, zerr := zenity.SelectFileSave(
					zenity.Title("保存加密的账号导出文件"),
					zenity.Filename("s3client-accounts.enc"),
					zenity.ConfirmOverwrite(),
				)
				if zerr != nil {
					return
				}
				werr := os.WriteFile(savePath, data, 0600)
				fyne.Do(func() {
					if werr != nil {
						dialog.ShowError(werr, w)
						return
					}
					showInfo("完成", "账号已加密导出", w)
				})
			}()
		})
	})
	importItem := fyne.NewMenuItem("导入账号...", func() {
		go func() {
			path, zerr := zenity.SelectFile(zenity.Title("选择账号导出文件"))
			if zerr != nil {
				return
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				fyne.Do(func() { dialog.ShowError(rerr, w) })
				return
			}
			fyne.Do(func() {
				askPassword(w, "导入账号 — 输入主密码", func(pw string) {
					go func() {
						n, ierr := s.ImportAccounts(pw, data)
						fyne.Do(func() {
							if ierr != nil {
								dialog.ShowError(ierr, w)
								return
							}
							reload()
							showInfo("完成", fmt.Sprintf("成功导入 %d 个账号", n), w)
						})
					}()
				})
			})
		}()
	})
	selectedAcc := func() (model.Account, bool) {
		if selected < 0 || selected >= len(accounts) {
			showInfo("提示", "请先选择一个账号", w)
			return model.Account{}, false
		}
		return accounts[selected], true
	}
	addItem := fyne.NewMenuItem("新增账号...", func() { editAccount(w, s, nil, reload) })
	editItem := fyne.NewMenuItem("编辑账号...", func() {
		if acc, ok := selectedAcc(); ok {
			doEdit(acc)
		}
	})
	delItem := fyne.NewMenuItem("删除账号", func() {
		if acc, ok := selectedAcc(); ok {
			doDelete(acc)
		}
	})
	testItem := fyne.NewMenuItem("测试连接", func() {
		if acc, ok := selectedAcc(); ok {
			doTest(acc)
		}
	})
	loadShareItem := fyne.NewMenuItem("加载分享配置...", func() { loadShare(w, func(sess *share.Session) {
		sessions = append(sessions, sess)
		reload()
	}) })
	shareMenu := fyne.NewMenu("分享", loadShareItem)
	if s == nil {
		// 未解锁账号库：只提供分享浏览与回到解锁界面
		unlockItem := fyne.NewMenuItem("解锁账号库...", func() { showUnlock(w) })
		w.SetMainMenu(fyne.NewMainMenu(fyne.NewMenu("文件", unlockItem), shareMenu))
	} else {
		accountMenu := fyne.NewMenu("账号", addItem, editItem, delItem, testItem,
			fyne.NewMenuItemSeparator(), exportItem, importItem)
		securityMenu := fyne.NewMenu("安全", lockItem, changePwItem)
		storageMenu := fyne.NewMenu("存储", locItem)
		w.SetMainMenu(fyne.NewMainMenu(accountMenu, shareMenu, securityMenu, storageMenu))
	}

	split := container.NewHSplit(leftPanel, rightPanel)
	split.Offset = 0.3
	w.SetContent(split)
}

// showBuckets 在右侧面板显示某账号下的 Bucket 列表。
func showBuckets(w fyne.Window, setRight func(fyne.CanvasObject), s *store.Store, acc *model.Account) {
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
				accessPlain := "可访问"
				var onOpen func()
				rowColor := color.Color(color.NRGBA{R: 0x2e, G: 0xa0, B: 0x43, A: 0xff}) // 绿：可访问
				if b.Accessible {
					onOpen = func() {
						pfx, _ := s.GetBucketPrefix(acc.ID, b.Name)
						showObjects(w, setRight, s, acc, b.Name, pfx)
					}
				} else {
					status = "✗ 无权限"
					accessPlain = "无权限"
					rowColor = color.NRGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xff} // 灰：无权限，onOpen 保持 nil 不能双击
				}
				row := newTappableRowDetail(b.Name, []string{"", "", status}, theme.StorageIcon(), nil, onOpen).
					withColor(rowColor).
					withSecondary(func() {
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
func showObjects(w fyne.Window, setRight func(fyne.CanvasObject), s *store.Store, acc *model.Account, bucket, prefix string) {
	loading := widget.NewLabel(fmt.Sprintf("正在加载 %s/%s ...", bucket, prefix))
	setRight(loading)

	go func() {
		objects, err := awss3.ListObjects(acc, bucket, prefix)
		// 只读分享没有账号库记录，也不记录上次目录。
		if err == nil && s != nil && !acc.ReadOnly {
			_ = s.SetBucketPrefix(acc.ID, bucket, prefix)
		}
		fyne.Do(func() {
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			buildObjectView(w, setRight, s, acc, bucket, prefix, objects, errMsg)
		})
	}()
}

// buildBreadcrumb 构建可点击的面包屑路径栏：bucket / seg1 / seg2 / ...
func buildBreadcrumb(w fyne.Window, setRight func(fyne.CanvasObject), s *store.Store, acc *model.Account, bucket, prefix string) fyne.CanvasObject {
	crumbs := container.NewHBox()
	bucketLink := widget.NewHyperlink(bucket, nil)
	bucketLink.OnTapped = func() { showObjects(w, setRight, s, acc, bucket, "") }
	crumbs.Add(bucketLink)

	if prefix != "" {
		parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
		for i, part := range parts {
			crumbs.Add(widget.NewLabel("/"))
			seg := strings.Join(parts[:i+1], "/") + "/"
			link := widget.NewHyperlink(part, nil)
			link.OnTapped = func() { showObjects(w, setRight, s, acc, bucket, seg) }
			crumbs.Add(link)
		}
	}
	return crumbs
}

// transferTask 描述一个并行传输任务（上传或下载）。
type transferTask struct {
	name string
	run  func(progress func(done, total int64)) error
}

// 传输进度行的尺寸：整行高度与底部细线高度。
const (
	transferRowH = 24
	thinBarH     = 4
)

// transferRow 一个紧凑的传输进度行：上排文件名与百分比，底部一条细线进度条。
type transferRow struct {
	widget.BaseWidget
	title string
	name  *widget.Label
	pct   *widget.Label
	value float64
	size  fyne.Size
	bg    *canvas.Rectangle
	fg    *canvas.Rectangle
}

func newTransferRow(title string) *transferRow {
	r := &transferRow{title: title}
	r.ExtendBaseWidget(r)
	return r
}

func (r *transferRow) CreateRenderer() fyne.WidgetRenderer {
	r.name = widget.NewLabel(r.title)
	r.name.Truncation = fyne.TextTruncateEllipsis
	r.pct = widget.NewLabel("0%")
	r.pct.Alignment = fyne.TextAlignTrailing
	r.bg = canvas.NewRectangle(color.NRGBA{R: 0x33, G: 0x38, B: 0x44, A: 0xff})
	r.fg = canvas.NewRectangle(theme.Color(theme.ColorNamePrimary))
	return &transferRowRenderer{row: r}
}

// setValue 更新进度（0~1）与百分比文字。
func (r *transferRow) setValue(v float64, pct string) {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	r.value = v
	if r.pct != nil {
		r.pct.SetText(pct)
	}
	r.Refresh()
}

type transferRowRenderer struct {
	row *transferRow
}

func (rr *transferRowRenderer) Destroy() {}

func (rr *transferRowRenderer) MinSize() fyne.Size {
	return fyne.NewSize(120, transferRowH)
}

func (rr *transferRowRenderer) Layout(size fyne.Size) {
	rr.row.size = size
	pctW := float32(48)
	topH := size.Height - thinBarH - 2
	rr.row.pct.Resize(fyne.NewSize(pctW, topH))
	rr.row.pct.Move(fyne.NewPos(size.Width-pctW, 0))
	rr.row.name.Resize(fyne.NewSize(size.Width-pctW-4, topH))
	rr.row.name.Move(fyne.NewPos(0, 0))
	rr.row.bg.Resize(fyne.NewSize(size.Width, thinBarH))
	rr.row.bg.Move(fyne.NewPos(0, size.Height-thinBarH))
	rr.layoutBar()
}

func (rr *transferRowRenderer) layoutBar() {
	w := rr.row.size.Width * float32(rr.row.value)
	rr.row.fg.Resize(fyne.NewSize(w, thinBarH))
	rr.row.fg.Move(fyne.NewPos(0, rr.row.size.Height-thinBarH))
	rr.row.fg.Hidden = rr.row.value <= 0
}

func (rr *transferRowRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{rr.row.bg, rr.row.fg, rr.row.name, rr.row.pct}
}

func (rr *transferRowRenderer) Refresh() {
	rr.layoutBar()
	rr.row.bg.Refresh()
	rr.row.fg.Refresh()
}

// runParallelTransfers 并行执行多个传输任务，在 progressArea 中显示每任务一条进度条，限流 maxConcurrent 并发。
func runParallelTransfers(w fyne.Window, progressArea *fyne.Container, tasks []transferTask, onDone func()) {
	if len(tasks) == 0 {
		return
	}
	const maxConcurrent = 4
	rows := make([]*transferRow, len(tasks))
	progressArea.Objects = nil
	for i, t := range tasks {
		row := newTransferRow(t.name)
		rows[i] = row
		progressArea.Add(row)
	}
	progressArea.Refresh()

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)
	errs := make([]error, len(tasks))
	for i := range tasks {
		idx := i
		t := tasks[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := t.run(func(done, total int64) {
				if total > 0 {
					ratio := float64(done) / float64(total)
					fyne.Do(func() {
						rows[idx].setValue(ratio, fmt.Sprintf("%d%%", int(ratio*100)))
					})
				}
			})
			errs[idx] = err
			if err == nil {
				fyne.Do(func() {
					rows[idx].setValue(1, "100%")
				})
			}
		}()
	}
	go func() {
		wg.Wait()
		fails := 0
		for _, e := range errs {
			if e != nil {
				fails++
			}
		}
		fyne.Do(func() {
			progressArea.Objects = nil
			progressArea.Refresh()
			if onDone != nil {
				onDone()
			}
			if fails > 0 {
				showInfo("完成", fmt.Sprintf("%d 个任务完成，%d 个失败", len(tasks)-fails, fails), w)
			} else {
				showInfo("完成", fmt.Sprintf("全部 %d 个任务完成", len(tasks)), w)
			}
		})
	}()
}

// buildObjectView 构建对象浏览器：面包屑 + 过滤框 + 文件/文件夹列表 + 底部操作栏 + 进度条。
func buildObjectView(w fyne.Window, setRight func(fyne.CanvasObject), s *store.Store, acc *model.Account, bucket, prefix string, objects []awss3.Object, errMsg string) {
	readOnly := acc.ReadOnly
	if readOnly && acc.ShareExpired() {
		setRight(widget.NewLabel(fmt.Sprintf("🔒 该分享配置的有效期已于 %s 结束，无法继续访问。",
			acc.ShareExpiresAt.Local().Format("2006-01-02 15:04"))))
		return
	}

	var rows []*tappableRow
	var selectedObj *awss3.Object
	selectedKeys := map[string]*awss3.Object{}

	selectRow := func(idx int, obj *awss3.Object) {
		for i, r := range rows {
			r.setSelected(i == idx)
		}
		selectedObj = obj
	}

	refresh := func() { showObjects(w, setRight, s, acc, bucket, prefix) }

	list := container.NewVBox()

	// 可过滤的行：名称预先转小写，过滤时只切换显隐，避免每次输入都重建整列表。
	type filterRow struct {
		row  *tappableRow
		name string
	}
	var filterRows []filterRow

	// applyFilter 按关键字显示/隐藏对象行。
	applyFilter := func(lf string) {
		for _, fr := range filterRows {
			fr.row.Hidden = lf != "" && !strings.Contains(fr.name, lf)
		}
		list.Refresh()
	}

	buildList := func() {
		rows = nil
		list.Objects = nil
		filterRows = nil
		selectedObj = nil
		for k := range selectedKeys {
			delete(selectedKeys, k)
		}

		// 只读分享没有账号库，根目录无法回退到 bucket 列表，因此不显示返回行。
		if !(readOnly && prefix == "") {
			backBtn := newTappableRow("⬆ 返回上一级", theme.NavigateBackIcon(), nil, func() {
				if prefix == "" {
					showBuckets(w, setRight, s, acc)
					return
				}
				showObjects(w, setRight, s, acc, bucket, parentPrefix(prefix))
			})
			rows = append(rows, backBtn)
			list.Add(backBtn)
		}

		for _, o := range objects {
			obj := o
			idx := len(rows)
			if obj.IsDir {
				row := newTappableRowDetail(obj.Name, []string{"", "文件夹", ""}, theme.FolderIcon(),
					func() { selectRow(idx, &obj) },
					func() { showObjects(w, setRight, s, acc, bucket, obj.Key) },
				).withSecondary(func() {
					showProperties(w, "文件夹属性", [][2]string{
						{"名称", obj.Name},
						{"Key", obj.Key},
						{"类型", "文件夹"},
					})
				}).withCheck(func(checked bool) {
					if checked {
						selectedKeys[obj.Key] = &obj
					} else {
						delete(selectedKeys, obj.Key)
					}
					selectRow(idx, &obj)
				})
				rows = append(rows, row)
				filterRows = append(filterRows, filterRow{row: row, name: strings.ToLower(obj.Name)})
				list.Add(row)
			} else {
				meta := []string{
					humanSize(obj.Size),
					obj.Type,
					obj.LastModified.Local().Format("2006-01-02 15:04"),
				}
				row := newTappableRowDetail(obj.Name, meta, theme.FileIcon(),
					func() {
						selectRow(idx, &obj)
					}, nil).withSecondary(func() {
					showProperties(w, "文件属性", [][2]string{
						{"名称", obj.Name},
						{"Key", obj.Key},
						{"大小", humanSize(obj.Size)},
						{"类型", obj.Type},
						{"最后修改", obj.LastModified.Local().Format("2006-01-02 15:04:05")},
					})
				}).withCheck(func(checked bool) {
					if checked {
						selectedKeys[obj.Key] = &obj
					} else {
						delete(selectedKeys, obj.Key)
					}
					selectRow(idx, &obj)
				})
				rows = append(rows, row)
				filterRows = append(filterRows, filterRow{row: row, name: strings.ToLower(obj.Name)})
				list.Add(row)
			}
		}
		list.Refresh()
	}
	buildList()

	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("输入关键字过滤...")
	var filterTimer *time.Timer
	filterEntry.OnChanged = func(text string) {
		// 输入防抖：停止输入约 150ms 后才真正过滤，避免每个字符都重排整列表。
		if filterTimer != nil {
			filterTimer.Stop()
		}
		filterTimer = time.AfterFunc(150*time.Millisecond, func() {
			fyne.Do(func() { applyFilter(strings.ToLower(text)) })
		})
	}

	nameHeader := widget.NewLabelWithStyle("名称", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	metaHeader := newMetaCols([]string{"大小", "类型", "修改时间"}, true)
	header := container.NewBorder(nil, nil, nameHeader, metaHeader)

	progressArea := container.NewVBox()
	progressScroll := container.NewVScroll(progressArea)
	progressScroll.SetMinSize(fyne.NewSize(0, 60))

	refreshBtn := widget.NewButtonWithIcon("刷新", theme.ViewRefreshIcon(), refresh)

	uploadBtn := widget.NewButtonWithIcon("上传文件", theme.UploadIcon(), func() {
		go func() {
			paths, err := zenity.SelectFileMultiple(zenity.Title("选择要上传的文件（可多选）"))
			if err != nil || len(paths) == 0 {
				return
			}
			tasks := make([]transferTask, 0, len(paths))
			for _, p := range paths {
				lp := p
				key := prefix + filepath.Base(lp)
				tasks = append(tasks, transferTask{
					name: filepath.Base(lp),
					run: func(progress func(done, total int64)) error {
						return awss3.UploadFile(acc, bucket, key, lp, progress)
					},
				})
			}
			fyne.Do(func() {
				runParallelTransfers(w, progressArea, tasks, refresh)
			})
		}()
	})

	downloadBtn := widget.NewButtonWithIcon("下载", theme.DownloadIcon(), func() {
		if len(selectedKeys) == 0 {
			showInfo("提示", "请先勾选要下载的文件或文件夹（行左侧复选框）", w)
			return
		}
		if !checkShareValid(w, acc) {
			return
		}
		picked := make([]*awss3.Object, 0, len(selectedKeys))
		for _, o := range selectedKeys {
			picked = append(picked, o)
		}
		go func() {
			dir, err := zenity.SelectFile(zenity.Title("选择下载到的目录"), zenity.Directory())
			if err != nil || dir == "" {
				return
			}
			tasks := make([]transferTask, 0, len(picked))
			firstPath := ""
			addTask := func(display, key, local string) {
				if firstPath == "" {
					firstPath = local
				}
				tasks = append(tasks, transferTask{
					name: display,
					run: func(progress func(done, total int64)) error {
						return awss3.DownloadFile(acc, bucket, key, local, progress)
					},
				})
			}
			for _, o := range picked {
				obj := o
				if !obj.IsDir {
					addTask(obj.Name, obj.Key, filepath.Join(dir, filepath.FromSlash(obj.Name)))
					continue
				}
				// 文件夹：递归列出其内部所有文件，按相对路径落盘（保留子目录结构）。
				items, lerr := awss3.ListObjectsRecursive(acc, bucket, obj.Key)
				if lerr != nil {
					fyne.Do(func() { dialog.ShowError(lerr, w) })
					return
				}
				// 下载结果形如 <目标目录>/<文件夹名>/<相对路径>
				base := strings.TrimSuffix(obj.Key, "/")
				if i := strings.LastIndex(base, "/"); i >= 0 {
					base = base[:i+1]
				} else {
					base = ""
				}
				for _, it := range items {
					rel := strings.TrimPrefix(it.Key, base)
					addTask(rel, it.Key, filepath.Join(dir, filepath.FromSlash(rel)))
				}
			}
			if len(tasks) == 0 {
				fyne.Do(func() { showInfo("提示", "所选文件夹内没有可下载的文件", w) })
				return
			}
			first := firstPath
			fyne.Do(func() {
				runParallelTransfers(w, progressArea, tasks, func() { revealInExplorer(first) })
			})
		}()
	})

	deleteBtn := widget.NewButtonWithIcon("删除文件", theme.DeleteIcon(), func() {
		if selectedObj == nil {
			showInfo("提示", "请先选择一个对象", w)
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
			showInfo("提示", "请先选择一个文件", w)
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

	// 只读分享只提供浏览与下载，不给上传/删除/新建文件夹的入口；
	// awss3 层对只读账号还有一道写入拦截作为兜底。
	bar := []fyne.CanvasObject{refreshBtn, downloadBtn, urlBtn}
	if !readOnly {
		shareBtn := widget.NewButtonWithIcon("分享只读", theme.MailSendIcon(), func() {
			showCreateShare(w, acc, bucket)
		})
		bar = []fyne.CanvasObject{refreshBtn, uploadBtn, downloadBtn, deleteBtn, urlBtn, mkdirBtn, shareBtn}
	}
	actionBar := container.NewHBox(bar...)

	msgLabel := widget.NewLabel("")
	msgLabel.Wrapping = fyne.TextWrapWord
	if errMsg != "" {
		msgLabel.SetText("⚠ 加载失败: " + errMsg)
	}
	msgScroll := container.NewVScroll(msgLabel)
	msgScroll.SetMinSize(fyne.NewSize(0, 60))
	bottom := container.NewVBox(actionBar, progressScroll, msgScroll)

	breadcrumb := buildBreadcrumb(w, setRight, s, acc, bucket, prefix)
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

// spConfig 把 model.Account 转成 sharepoint.Config。
func spConfig(acc *model.Account) *sharepoint.Config {
	return &sharepoint.Config{
		TenantID:     acc.TenantID,
		ClientID:     acc.ClientID,
		ClientSecret: acc.ClientSecret,
		SiteURL:      acc.SiteURL,
	}
}

// showSharePointDrives 在右侧面板显示 SharePoint 账号下的文档库列表。
func showSharePointDrives(w fyne.Window, setRight func(fyne.CanvasObject), s *store.Store, acc *model.Account) {
	loading := widget.NewLabel(fmt.Sprintf("正在加载 %s 的文档库...", acc.Name))
	setRight(loading)

	go func() {
		cfg := spConfig(acc)
		drives, err := sharepoint.ListDrives(context.Background(), cfg)
		fyne.Do(func() {
			if err != nil {
				setRight(widget.NewLabel(fmt.Sprintf("加载失败: %v", err)))
				return
			}
			list := container.NewVBox()
			for _, d := range drives {
				drv := d
				row := newTappableRow(drv.Name, theme.StorageIcon(), nil,
					func() { showSharePointItems(w, setRight, s, acc, drv.ID, drv.Name, "") },
				)
				list.Add(row)
			}
			title := widget.NewLabelWithStyle(
				fmt.Sprintf("账号 %s — 共 %d 个文档库（双击进入）", acc.Name, len(drives)),
				fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			setRight(container.NewBorder(title, nil, nil, nil, container.NewVScroll(list)))
		})
	}()
}

// showSharePointItems 加载 SharePoint 文档库中某路径下的内容并构建浏览器视图。
func showSharePointItems(w fyne.Window, setRight func(fyne.CanvasObject), s *store.Store, acc *model.Account, driveID, driveName, itemPath string) {
	loading := widget.NewLabel(fmt.Sprintf("正在加载 %s/%s ...", driveName, itemPath))
	setRight(loading)

	go func() {
		cfg := spConfig(acc)
		items, err := sharepoint.ListChildren(context.Background(), cfg, driveID, itemPath)
		fyne.Do(func() {
			buildSharePointView(w, setRight, s, acc, driveID, driveName, itemPath, items, err)
		})
	}()
}

// buildSharePointView 构建 SharePoint 文件浏览器视图。
func buildSharePointView(w fyne.Window, setRight func(fyne.CanvasObject), s *store.Store, acc *model.Account, driveID, driveName, itemPath string, items []sharepoint.Item, loadErr error) {
	var rows []*tappableRow
	selectedKeys := map[string]*sharepoint.Item{}

	selectRow := func(idx int) {
		for i, r := range rows {
			r.setSelected(i == idx)
		}
	}

	list := container.NewVBox()

	// 返回按钮
	backBtn := newTappableRow("⬆ 返回上一级", theme.NavigateBackIcon(), nil, func() {
		if itemPath == "" {
			showSharePointDrives(w, setRight, s, acc)
			return
		}
		showSharePointItems(w, setRight, s, acc, driveID, driveName, spParentPath(itemPath))
	})
	rows = append(rows, backBtn)
	list.Add(backBtn)

	if loadErr != nil {
		list.Add(widget.NewLabel("⚠ 加载失败: " + loadErr.Error()))
	}

	for _, it := range items {
		item := it
		idx := len(rows)
		if item.IsFolder {
			row := newTappableRowDetail(item.Name, []string{"", "文件夹", ""}, theme.FolderIcon(),
				func() { selectRow(idx) },
				func() {
					subPath := itemPath
					if subPath != "" && !strings.HasSuffix(subPath, "/") {
						subPath += "/"
					}
					subPath += item.Name
					showSharePointItems(w, setRight, s, acc, driveID, driveName, subPath)
				},
			)
			rows = append(rows, row)
			list.Add(row)
		} else {
			meta := []string{
				humanSize(item.Size),
				"",
				item.LastModified.Local().Format("2006-01-02 15:04"),
			}
			row := newTappableRowDetail(item.Name, meta, theme.FileIcon(),
				func() {
					selectRow(idx)
				}, nil).withCheck(func(checked bool) {
				if checked {
					selectedKeys[item.ID] = &item
				} else {
					delete(selectedKeys, item.ID)
				}
				selectRow(idx)
			})
			rows = append(rows, row)
			list.Add(row)
		}
	}

	// 面包屑
	crumbs := container.NewHBox()
	driveLink := widget.NewHyperlink(driveName, nil)
	driveLink.OnTapped = func() { showSharePointItems(w, setRight, s, acc, driveID, driveName, "") }
	crumbs.Add(driveLink)
	if itemPath != "" {
		parts := strings.Split(strings.TrimSuffix(itemPath, "/"), "/")
		for i, part := range parts {
			crumbs.Add(widget.NewLabel("/"))
			seg := strings.Join(parts[:i+1], "/")
			link := widget.NewHyperlink(part, nil)
			link.OnTapped = func() { showSharePointItems(w, setRight, s, acc, driveID, driveName, seg) }
			crumbs.Add(link)
		}
	}

	nameHeader := widget.NewLabelWithStyle("名称", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	metaHeader := newMetaCols([]string{"大小", "", "修改时间"}, true)
	header := container.NewBorder(nil, nil, nameHeader, metaHeader)

	progressArea := container.NewVBox()
	progressScroll := container.NewVScroll(progressArea)
	progressScroll.SetMinSize(fyne.NewSize(0, 60))

	refreshBtn := widget.NewButtonWithIcon("刷新", theme.ViewRefreshIcon(), func() {
		showSharePointItems(w, setRight, s, acc, driveID, driveName, itemPath)
	})
	downloadBtn := widget.NewButtonWithIcon("下载文件", theme.DownloadIcon(), func() {
		if len(selectedKeys) == 0 {
			showInfo("提示", "请先勾选要下载的文件（文件行左侧复选框）", w)
			return
		}
		picked := make([]*sharepoint.Item, 0, len(selectedKeys))
		for _, it := range selectedKeys {
			picked = append(picked, it)
		}
		go func() {
			dir, err := zenity.SelectFile(zenity.Title("选择下载到的目录"), zenity.Directory())
			if err != nil || dir == "" {
				return
			}
			cfg := spConfig(acc)
			tasks := make([]transferTask, 0, len(picked))
			for _, it := range picked {
				item := it
				savePath := filepath.Join(dir, item.Name)
				tasks = append(tasks, transferTask{
					name: item.Name,
					run: func(progress func(done, total int64)) error {
						return sharepoint.DownloadItem(context.Background(), cfg, driveID, item.ID, savePath, progress)
					},
				})
			}
			firstPath := filepath.Join(dir, picked[0].Name)
			fyne.Do(func() {
				runParallelTransfers(w, progressArea, tasks, func() { revealInExplorer(firstPath) })
			})
		}()
	})

	actionBar := container.NewHBox(refreshBtn, downloadBtn)
	bottom := container.NewVBox(actionBar, progressScroll)
	top := container.NewVBox(crumbs, header)
	setRight(container.NewBorder(top, bottom, nil, nil, container.NewVScroll(list)))
}

// spParentPath 返回 SharePoint 路径的上一级，如 "a/b/c" -> "a/b"。
func spParentPath(p string) string {
	p = strings.TrimSuffix(p, "/")
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[:idx]
	}
	return ""
}

// editAccount 弹出账号编辑表单；acc 为 nil 表示新增。支持 S3 和 SharePoint 两种类型。
func editAccount(w fyne.Window, s *store.Store, acc *model.Account, onDone func()) {
	name := widget.NewEntry()

	// S3 字段
	endpoint := widget.NewEntry()
	endpoint.SetPlaceHolder("留空则使用 AWS 默认端点")
	region := widget.NewEntry()
	region.SetPlaceHolder("如 us-east-1")
	accessKey := widget.NewEntry()
	secretKey := widget.NewPasswordEntry()
	pathStyle := widget.NewCheck("使用 Path-Style (兼容 MinIO 等)", nil)

	// SharePoint 字段
	tenantID := widget.NewEntry()
	tenantID.SetPlaceHolder("Azure AD 租户 ID")
	clientID := widget.NewEntry()
	clientID.SetPlaceHolder("应用注册的 Client ID")
	clientSecret := widget.NewPasswordEntry()
	clientSecret.SetPlaceHolder("客户端密钥")
	siteURL := widget.NewEntry()
	siteURL.SetPlaceHolder("如 https://contoso.sharepoint.com/sites/TeamA")

	s3Items := []*widget.FormItem{
		widget.NewFormItem("Endpoint", endpoint),
		widget.NewFormItem("Region", region),
		widget.NewFormItem("Access Key ID", accessKey),
		widget.NewFormItem("Secret Access Key", secretKey),
		widget.NewFormItem("", pathStyle),
	}
	spItems := []*widget.FormItem{
		widget.NewFormItem("Tenant ID", tenantID),
		widget.NewFormItem("Client ID", clientID),
		widget.NewFormItem("Client Secret", clientSecret),
		widget.NewFormItem("Site URL", siteURL),
	}

	s3Form := widget.NewForm(s3Items...)
	spForm := widget.NewForm(spItems...)
	spForm.Hidden = true

	providerSelect := widget.NewSelect([]string{"S3", "SharePoint"}, func(val string) {
		if val == "SharePoint" {
			s3Form.Hidden = true
			spForm.Hidden = false
		} else {
			s3Form.Hidden = false
			spForm.Hidden = true
		}
		s3Form.Refresh()
		spForm.Refresh()
	})
	providerSelect.SetSelected("S3")

	isEdit := acc != nil
	if isEdit {
		name.SetText(acc.Name)
		if acc.IsSharePoint() {
			providerSelect.SetSelected("SharePoint")
			tenantID.SetText(acc.TenantID)
			clientID.SetText(acc.ClientID)
			clientSecret.SetText(acc.ClientSecret)
			siteURL.SetText(acc.SiteURL)
		} else {
			providerSelect.SetSelected("S3")
			endpoint.SetText(acc.Endpoint)
			region.SetText(acc.Region)
			accessKey.SetText(acc.AccessKeyID)
			secretKey.SetText(acc.SecretAccessKey)
			pathStyle.SetChecked(acc.UsePathStyle)
		}
	}

	content := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("名称"), nil, name),
		container.NewBorder(nil, nil, widget.NewLabel("类型"), nil, providerSelect),
		s3Form,
		spForm,
	)
	scroll := container.NewVScroll(content)
	scroll.SetMinSize(fyne.NewSize(460, 320))

	title := "新增账号"
	if isEdit {
		title = "编辑账号"
	}

	dialog.ShowCustomConfirm(title, "保存", "取消", scroll, func(ok bool) {
		if !ok {
			return
		}
		if name.Text == "" {
			showInfo("提示", "名称不能为空", w)
			return
		}
		a := &model.Account{Name: name.Text}
		if providerSelect.Selected == "SharePoint" {
			a.Provider = "sharepoint"
			if tenantID.Text == "" || clientID.Text == "" || clientSecret.Text == "" || siteURL.Text == "" {
				showInfo("提示", "SharePoint 的所有字段均不能为空", w)
				return
			}
			a.TenantID = tenantID.Text
			a.ClientID = clientID.Text
			a.ClientSecret = clientSecret.Text
			a.SiteURL = siteURL.Text
		} else {
			a.Provider = "s3"
			if accessKey.Text == "" {
				showInfo("提示", "Access Key ID 不能为空", w)
				return
			}
			a.Endpoint = endpoint.Text
			a.Region = region.Text
			a.AccessKeyID = accessKey.Text
			a.SecretAccessKey = secretKey.Text
			a.UsePathStyle = pathStyle.Checked
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

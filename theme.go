package main

import (
	"fmt"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// MailfsTheme — 深色工业风主题
// 主色：#00D4AA（青绿）配 #0a0c0f 底色
type MailfsTheme struct{}

var _ fyne.Theme = (*MailfsTheme)(nil)

func (t *MailfsTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	switch n {
	case theme.ColorNameBackground:
		return hex("#0f1114")
	case theme.ColorNameButton:
		return hex("#1a1e26")
	case theme.ColorNameDisabledButton:
		return hex("#151820")
	case theme.ColorNamePrimary:
		return hex("#00D4AA")
	case theme.ColorNameFocus:
		return hexA("#00D4AA", 0x70)
	case theme.ColorNameHover:
		return hexA("#00D4AA", 0x18)
	case theme.ColorNameInputBackground:
		return hex("#161a22")
	case theme.ColorNameShadow:
		return hexA("#000000", 0x88)
	case theme.ColorNameHeaderBackground:
		return hex("#12161c")
	case theme.ColorNameMenuBackground:
		return hex("#161a22")
	case theme.ColorNameOverlayBackground:
		return hexA("#0f1114", 0xee)
	case theme.ColorNameScrollBar:
		return hexA("#00D4AA", 0x44)
	case theme.ColorNameSeparator:
		return hex("#1e2632")
	case theme.ColorNameForeground:
		return hex("#c4d0dc")
	case theme.ColorNameDisabled:
		return hex("#4a5666")
	case theme.ColorNamePlaceHolder:
		return hex("#4a5666")
	case theme.ColorNameSuccess:
		return hex("#00D4AA")
	case theme.ColorNameWarning:
		return hex("#FFAA00")
	case theme.ColorNameError:
		return hex("#FF4060")
	case theme.ColorNameSelection:
		return hexA("#00D4AA", 0x2a)
	}
	return theme.DefaultTheme().Color(n, v)
}

func (t *MailfsTheme) Font(s fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(s)
}

func (t *MailfsTheme) Icon(n fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(n)
}

func (t *MailfsTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNamePadding:
		return 6
	case theme.SizeNameInnerPadding:
		return 8
	case theme.SizeNameText:
		return 13
	case theme.SizeNameHeadingText:
		return 18
	case theme.SizeNameSubHeadingText:
		return 15
	case theme.SizeNameCaptionText:
		return 11
	case theme.SizeNameInputBorder:
		return 1.5
	case theme.SizeNameScrollBar:
		return 4
	}
	return theme.DefaultTheme().Size(n)
}

// helpers
func hex(s string) color.Color {
	var r, g, b uint8
	fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b)
	return color.NRGBA{R: r, G: g, B: b, A: 0xff}
}

func hexA(s string, a uint8) color.Color {
	c := hex(s).(color.NRGBA)
	c.A = a
	return c
}

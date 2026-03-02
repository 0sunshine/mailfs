package main

import (
	"fyne.io/fyne/v2/app"
	"mailfs/guiapp"
)

func main() {
	a := app.New()
	a.Settings().SetTheme(&MailfsTheme{})

	w := guiapp.NewMainWindow(a)
	w.ShowAndRun()
}

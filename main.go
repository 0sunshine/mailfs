package main

import (
	"fmt"
	"mailfs/guiapp"
	"mailfs/libfs"
	"os"
	"strings"

	"fyne.io/fyne/v2/app"
)

func main() {
	fs := &libfs.MailFileSystem{}

	lines, err := readPasswd("passwd.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "read passwd.txt error: %v\n", err)
		os.Exit(1)
	}

	if err := fs.Login(lines[0], lines[1]); err != nil {
		fmt.Fprintf(os.Stderr, "login error: %v\n", err)
		os.Exit(1)
	}

	a := app.New()
	a.Settings().SetTheme(&MailfsTheme{})

	w := guiapp.NewMainWindow(a, fs)
	w.ShowAndRun()
}

func readPasswd(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("passwd.txt 格式错误")
	}
	lines[0] = strings.TrimRight(lines[0], "\r\n")
	lines[1] = strings.TrimRight(lines[1], "\r\n")
	return lines, nil
}

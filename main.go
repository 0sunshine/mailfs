package main

import (
	"fmt"
	"fyne.io/fyne/v2/app"
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
	"mailfs/guiapp"
	"os"
)

var logFile *lumberjack.Logger

func init_log() {
	// Output to stdout instead of the default stderr
	// Can be any io.Writer, see below for File example
	logFile = &lumberjack.Logger{
		Filename:   "log.txt",
		MaxSize:    50, // MB
		MaxBackups: 10,
		MaxAge:     28, // days
		Compress:   false,
	}

	logrus.SetFormatter(&logrus.TextFormatter{
		ForceColors:   true,
		DisableQuote:  true, // 禁用引号，防止换行符被转义
		FullTimestamp: true,
	})

	logrus.SetOutput(logFile)
	logrus.SetOutput(os.Stdout)

	// Only log the warning severity or above.
	logrus.SetLevel(logrus.DebugLevel)

	logrus.SetReportCaller(true)
}

func main() {
	init_log()
	defer func() {
		err := logFile.Close()
		if err != nil {
			fmt.Println(err)
		}
	}()

	a := app.New()
	a.Settings().SetTheme(&MailfsTheme{})

	w := guiapp.NewMainWindow(a)
	w.ShowAndRun()
}

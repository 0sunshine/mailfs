package main

import (
	"fmt"
	"fyne.io/fyne/v2/app"
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
	"mailfs/guiapp"
	"mailfs/libfs"
	"os"
)

var logFile *lumberjack.Logger

func init_log(cfg *libfs.AppConfig) {
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

	// 根据配置决定日志输出目标
	if cfg.LogOutput == "file" {
		logrus.SetOutput(logFile)
	} else {
		logrus.SetOutput(os.Stdout)
	}

	logrus.SetLevel(logrus.DebugLevel)
	logrus.SetReportCaller(true)
}

func main() {
	// 加载统一配置（须在 init_log 之前，因为日志输出目标由配置决定）
	cfg := libfs.LoadConfig()
	init_log(cfg)
	defer func() {
		err := logFile.Close()
		if err != nil {
			fmt.Println(err)
		}
	}()

	// 启动 HTTP-to-IMAP 流媒体服务（后台），使用配置中的监听地址
	httpServer := libfs.NewHTTPIMAPServer(cfg.HTTPListenAddr)
	httpServer.StartAsync()
	logrus.Infof("HTTP streaming server started on %s", cfg.HTTPListenAddr)

	a := app.NewWithID("com.github.mailfs")
	a.Settings().SetTheme(&MailfsTheme{})

	w := guiapp.NewMainWindow(a)
	w.ShowAndRun()
}

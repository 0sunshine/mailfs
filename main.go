package main

import (
	"fmt"
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
	"mailfs/libfs"
	"os"
	"strings"
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

	usrpwd, err := os.ReadFile("passwd.txt")
	if err != nil {
		logrus.Fatalf("open file error: %v", err)
	}
	lines := strings.Split(string(usrpwd), "\n")

	lines[0] = strings.TrimRight(lines[0], "\r")
	lines[1] = strings.TrimRight(lines[1], "\r")

	fs := libfs.MailFileSystem{}
	fs.Login(lines[0], lines[1])

	fs.Enter("其他文件夹/测试")

	//err = fs.CacheCurrDir()
	//if err != nil {
	//	logrus.Errorf("failed to CacheCurrDir: %v\n", err)
	//	return
	//}

	err = fs.SetDownloadRootDir("d:/")
	if err != nil {
		logrus.Errorf("failed to SetDownloadRootDir: %v\n", err)
		return
	}

	files, err := fs.GetCacheFiles()
	if err != nil {
		logrus.Errorf("failed to GetCacheFiles: %v\n", err)
		return
	}
	logrus.Infof("GetCacheFiles len: %v\n", len(files))

	for _, f := range files {
		fs.DownloadCacheFile(f)
	}

	//fs.UploadFile("G:/BaiduNetdiskDownload/Vue3实战商城后台管理系统开发/Vue3实战商城后台管理系统开发/20.部署服务器上线/[20.1]--部署前环境搭建【海量资源：vipc9.com】.mp4")

	//fs.UploadDir("G:\\BaiduNetdiskDownload\\121 - Nginx入门到实践Nginx中间件")

	logrus.Printf("exit ...")
}

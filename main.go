package main

import (
	"fmt"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
	"io"
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
	err = fs.CacheCurrDir()
	if err != nil {
		logrus.Errorf("failed to CacheCurrDir: %v\n", err)
	}

	//
	//fs.UploadFile("G:/BaiduNetdiskDownload/Vue3实战商城后台管理系统开发/Vue3实战商城后台管理系统开发/20.部署服务器上线/[20.1]--部署前环境搭建【海量资源：vipc9.com】.mp4")

	return

	c, err := imapclient.DialTLS("imap.qq.com:993", nil)
	if err != nil {
		logrus.Fatalf("failed to dial IMAP server: %v\n", err)
	}
	defer c.Close()

	if err := c.Login(lines[0], lines[1]).Wait(); err != nil {
		logrus.Fatalf("failed to login: %v\n", err)
	}

	mailboxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		logrus.Fatalf("failed to list mailboxes: %v\n", err)
	}
	logrus.Printf("Found %v mailboxes\n", len(mailboxes))
	for _, mbox := range mailboxes {
		logrus.Printf(" - %v", mbox.Mailbox)
	}

	selectedMbox, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		logrus.Fatalf("failed to select INBOX: %v", err)
	}
	logrus.Printf("INBOX contains %v messages", selectedMbox.NumMessages)

	data, err := c.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{
			{Key: "Subject", Value: "Optimizing"},
		},
	}, nil).Wait()
	if err != nil {
		logrus.Fatalf("UID SEARCH command failed: %v", err)
	}

	//logrus.Printf("seqs matching the search criteria: %v", data.AllSeqNums())
	logrus.Printf("UIDs matching the search criteria: %v", data.AllUIDs())

	//
	seqSet := imap.SeqSetNum(4)
	bodySection := &imap.FetchItemBodySection{}
	fetchOptions := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}
	fetchCmd := c.Fetch(seqSet, fetchOptions)
	defer fetchCmd.Close()

	for {
		msg := fetchCmd.Next()
		if msg == nil {
			break
		}

		for {
			item := msg.Next()
			if item == nil {
				break
			}

			switch real_item := item.(type) {
			case imapclient.FetchItemDataUID:
				logrus.Printf("UID: %v", real_item.UID)
			case imapclient.FetchItemDataBodySection:
				var bodySectionData imapclient.FetchItemDataBodySection
				bodySectionData, _ = item.(imapclient.FetchItemDataBodySection)
				mr, err := mail.CreateReader(bodySectionData.Literal)
				if err != nil {
					logrus.Fatalf("failed to create mail reader: %v", err)
				}

				// Process the message's parts
				for {
					p, err := mr.NextPart()
					if err == io.EOF {
						break
					} else if err != nil {
						logrus.Fatalf("failed to read message part: %v", err)
					}

					switch h := p.Header.(type) {
					case *mail.InlineHeader:
						// This is the message's text (can be plain-text or HTML)
						b, _ := io.ReadAll(p.Body)
						logrus.Printf("Inline text: %v", string(b))
					case *mail.AttachmentHeader:
						// This is an attachment
						filename, _ := h.Filename()

						logrus.Printf("Attachment: %v", filename)
						b, _ := io.ReadAll(p.Body)

						f, _ := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
						f.Write(b)
						f.Close()
					}
				}
			}
		}
	}

	//if err := fetchCmd.Close(); err != nil {
	//	logrus.Fatalf("FETCH command failed: %v", err)
	//}

	logrus.Printf("exit ...")
}

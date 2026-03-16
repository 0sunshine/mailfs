package libfs

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
	"github.com/sirupsen/logrus"
)

// 连接超时
const dialTimeout = 15 * time.Second

type MailFileSystem struct {
	c               *imapclient.Client
	remoteDir       string
	downloadRootDir string

	// 保存登录凭据，用于网络错误后自动重连
	usr string
	pwd string
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

func NewMailFileSystem() *MailFileSystem {
	fs := MailFileSystem{}

	lines, err := readPasswd("passwd.txt")
	if err != nil {
		logrus.Errorf("read passwd.txt error: %v\n", err)
		os.Exit(1)
	}

	if err := fs.Login(lines[0], lines[1]); err != nil {
		logrus.Errorf("login error: %v\n", err)
	}

	fs.SetDownloadRootDir("D:/")
	return &fs
}

// Login 带超时的登录：手动 TCP 拨号 + TLS 握手 + IMAP 登录，
// 防止网络异常时 DialTLS 无限阻塞。
func (mailfs *MailFileSystem) Login(usr string, pwd string) error {
	// ── 1. 带超时的 TCP 拨号 ──
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", "imap.qq.com:993")
	if err != nil {
		logrus.Errorf("TCP 拨号失败: %v", err)
		return fmt.Errorf("TCP 拨号失败: %w", err)
	}

	// ── 2. 带超时的 TLS 握手 ──
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "imap.qq.com"})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		logrus.Errorf("TLS 握手失败: %v", err)
		return fmt.Errorf("TLS 握手失败: %w", err)
	}

	// ── 3. 在已建立的 TLS 连接上创建 IMAP 客户端 ──
	c := imapclient.New(tlsConn, nil)
	if err != nil {
		tlsConn.Close()
		logrus.Errorf("创建 IMAP client 失败: %v", err)
		return fmt.Errorf("创建 IMAP client 失败: %w", err)
	}

	// ── 4. IMAP LOGIN ──
	if err = c.Login(usr, pwd).Wait(); err != nil {
		c.Close()
		logrus.Errorf("IMAP 登录失败: %v", err)
		return fmt.Errorf("IMAP 登录失败: %w", err)
	}

	mailfs.c = c
	mailfs.usr = usr
	mailfs.pwd = pwd

	return nil
}

func (mailfs *MailFileSystem) Logout() {
	mailfs.safeLogout()
}

func (mailfs *MailFileSystem) cacheUID(uids []imap.UID) error {

	logrus.Debugf("cacheUID : %v", uids)

	var mailText *MailText
	var msgUID imap.UID
	var messages []*imapclient.FetchMessageBuffer
	bodySection := &imap.FetchItemBodySection{Part: []int{1}}

	err := mailfs.withRetry("cacheUID/FETCH", func() error {
		uidSeqSet := imap.UIDSetNum(uids...)

		fetchOptions := &imap.FetchOptions{
			UID:         true,
			BodySection: []*imap.FetchItemBodySection{bodySection},
		}

		var err error

		logrus.Debugf("FETCH begin...")

		messages, err = mailfs.c.Fetch(uidSeqSet, fetchOptions).Collect()
		if err != nil {
			logrus.Errorf("FETCH command failed: %v", err)
			return err
		}

		if len(messages) <= 0 {
			logrus.Errorf("len(messages) <= 0")
			return errors.New("len(messages) <= 0")
		}

		logrus.Debugf("FETCH end...")

		return nil
	})

	if err != nil {
		return err
	}

	for _, msg := range messages {
		header := msg.FindBodySection(bodySection)
		r := quotedprintable.NewReader(bytes.NewReader(header))
		b, err := io.ReadAll(r)
		if err != nil {
			logrus.Errorf("io.ReadAll failed: %v", err)
			return err
		}

		mailText = MailTextFromByte(string(b))
		msgUID = msg.UID

		// 如果是加密邮件，解密 localpath 和 subject
		// 数据库中存储解密后的真实路径
		if IsEncryptedSubject(mailText.Vsubject) {
			mailText.Vlocalpath = Decrypt(mailText.Vlocalpath)
			mailText.Vsubject = DecryptSubject(mailText.Vsubject)
		}

		if len(mailText.Vsubject) == 0 || len(mailText.Vlocalpath) == 0 {
			logrus.Warnf("not a mailfs: %v, uid: %v", mailText.Vsubject, msgUID)
			return nil
		}

		err = cacheToDB(msgUID, mailText)
		if err != nil {
			logrus.Errorf("cacheToDB failed: %v", err)
			return err
		}
	}

	return nil
}

func (mailfs *MailFileSystem) CacheCurrDir() error {
	return mailfs.CacheCurrDirWithProgress(nil)
}

// CacheProgressFunc 同步缓存进度回调: (已完成UID数, 总UID数)
type CacheProgressFunc func(done, total int)

func (mailfs *MailFileSystem) CacheCurrDirWithProgress(progressCb CacheProgressFunc) error {
	if mailfs.c == nil {
		return errors.New("not login")
	}

	if len(mailfs.remoteDir) <= 0 {
		return errors.New("not select dir")
	}

	var allUIDs []imap.UID
	err := mailfs.withRetry("CacheCurrDir/SEARCH", func() error {
		uids, err := mailfs.c.UIDSearch(&imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{},
		}, nil).Wait()
		if err != nil {
			logrus.Errorf("UID SEARCH command failed: %v", err)
			return err
		}
		allUIDs = uids.AllUIDs()
		return nil
	})
	if err != nil {
		return err
	}

	logrus.Infof("UID has: %v", allUIDs)

	total := len(allUIDs)
	if progressCb != nil {
		progressCb(1, total)
	}

	const UidsCount = 32

	uids := make([]imap.UID, 0, UidsCount)

	for i, uid := range allUIDs {

		existed, err := isUIDCached(mailfs.remoteDir, int64(uid))
		if err != nil {
			return err
		}

		if existed {
			logrus.Debugf("cacheUID : %v has been cached, ignore ...", uid)
			continue
		}

		uids = append(uids, uid)
		if len(uids) >= UidsCount || i == (total-1) {
			err = mailfs.cacheUID(uids)
			if err != nil {
				logrus.Errorf("cacheUID failed: %v", err)
				return err
			}

			uids = make([]imap.UID, 0, UidsCount)

			if progressCb != nil {
				progressCb(i, total)
			}
		}
	}

	return nil
}

func (mailfs *MailFileSystem) Enter(remoteDir string) error {
	if mailfs.c == nil {
		return errors.New("not login")
	}

	selectedMbox, err := mailfs.c.Select(remoteDir, nil).Wait()
	if err != nil {
		logrus.Fatalf("failed to select : %v", remoteDir)
		return err
	}

	mailfs.remoteDir = remoteDir
	logrus.Infof("%v contains %v messages", remoteDir, selectedMbox.NumMessages)
	return nil
}

func (mailfs *MailFileSystem) SetDownloadRootDir(dir string) error {

	fileInfo, err := os.Stat(dir)
	if err != nil {
		logrus.Errorf("SetDownloadRootDir error: %v", err)
		return err
	}

	if !fileInfo.IsDir() {
		logrus.Errorf("not a dir: %v", dir)
		return errors.New("not a dir")
	}

	mailfs.downloadRootDir = dir
	if mailfs.downloadRootDir[len(mailfs.downloadRootDir)-1] != '/' {
		mailfs.downloadRootDir = mailfs.downloadRootDir + "/"
	}

	return nil
}

func (mailfs *MailFileSystem) GetCacheFiles() ([]CacheFile, error) {
	if mailfs.c == nil {
		return nil, errors.New("not login")
	}

	if len(mailfs.remoteDir) <= 0 {
		return nil, errors.New("not select dir")
	}

	return getCacheFileFromDB(mailfs.remoteDir, "")
}

func (mailfs *MailFileSystem) downloadUID(uid int64) (*MailText, []byte, error) {
	logrus.Debugf("downloadUID : %v", uid)

	var mailText *MailText
	var fileData []byte

	err := mailfs.withRetry("downloadUID/FETCH", func() error {
		uidSeqSet := imap.UIDSetNum(imap.UID(uid))
		bodySection := &imap.FetchItemBodySection{}
		fetchOptions := &imap.FetchOptions{
			UID:         true,
			BodySection: []*imap.FetchItemBodySection{bodySection},
		}

		fetchCmd := mailfs.c.Fetch(uidSeqSet, fetchOptions)
		defer fetchCmd.Close()

		msg := fetchCmd.Next()
		if msg == nil {
			return errors.New("no msg from fetch")
		}

		var bodyData imapclient.FetchItemDataBodySection
		ok := false

		for {
			item := msg.Next()
			if item == nil {
				break
			}

			bodyData, ok = item.(imapclient.FetchItemDataBodySection)
			if ok {
				break
			}
		}

		if !ok {
			return errors.New("FETCH command did not return body section")
		}

		mr, err := mail.CreateReader(bodyData.Literal)
		if err != nil {
			logrus.Errorf("failed to create mail reader: %v", err)
			return err
		}

		mailText = nil
		fileData = nil

		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			} else if err != nil {
				logrus.Errorf("failed to read message part: %v", err)
				return err
			}

			switch p.Header.(type) {
			case *mail.InlineHeader:
				b, err := io.ReadAll(p.Body)
				if err != nil {
					logrus.Errorf("failed to get inline: %v", err)
					return err
				}
				mailText = MailTextFromByte(string(b))
			case *mail.AttachmentHeader:
				fileData, err = io.ReadAll(p.Body)
				if err != nil {
					logrus.Errorf("failed to get attachment file: %v", err)
					return err
				}
			}
		}

		if mailText == nil || fileData == nil {
			logrus.Errorf("mailText == nil || fileData == nil")
			return errors.New("parse mail error")
		}

		if len(mailText.Vsubject) == 0 || len(mailText.Vlocalpath) == 0 {
			errStr := fmt.Sprintf("not a mailfs: %v, uid: %v", mailText.Vsubject, uid)
			logrus.Error(errStr)
			return errors.New(errStr)
		}

		return nil
	})

	// 下载时如果是加密邮件，解密路径信息
	if err == nil && mailText != nil && IsEncryptedSubject(mailText.Vsubject) {
		mailText.Vlocalpath = Decrypt(mailText.Vlocalpath)
		mailText.Vsubject = DecryptSubject(mailText.Vsubject)
	}

	return mailText, fileData, err
}

func (mailfs *MailFileSystem) DownloadCacheFile(f CacheFile) error {
	if mailfs.c == nil {
		return errors.New("not login")
	}

	var err error

	if mailfs.remoteDir != f.MailFolder {
		if err = mailfs.Enter(f.MailFolder); err != nil {
			logrus.Errorf("Enter error: %v", err)
			return errors.New("can't not enter dir")
		}
	}

	if f.Blocks == nil {
		if f.Blocks, err = getCacheBlockFromDB(f.FileID); err != nil {
			logrus.Errorf("getCacheBlockFromDB error: %v", err)
			return errors.New("getCacheBlockFromDB error")
		}
	}

	if int64(len(f.Blocks)) != f.BlockCount {
		errStr := fmt.Sprintf("error, because want block count(%v), but only cache(%v)", len(f.Blocks), f.BlockCount)
		logrus.Errorf("%v", errStr)
		return errors.New(errStr)
	}

	p := strings.Split(f.LocalPath, "/")
	p[0] = p[0][:1]
	savePath := strings.Join(p, "/")
	savePath = mailfs.downloadRootDir + savePath
	saveTmpPath := savePath + ".tmp"

	_, err = os.Stat(savePath)
	if err == nil {
		logrus.Debugf("file exist, %v", savePath)
		return nil
	}

	os.Remove(saveTmpPath)

	filename := filepath.Base(savePath)
	path := filepath.Dir(savePath)
	fileCachePath := path + "/mailfscache_" + f.FileMD5

	logrus.Infof("%s %s", filename, path)

	if err := os.MkdirAll(fileCachePath, 0755); err != nil {
		logrus.Errorf("mkdir fail: %v", err)
		return err
	}

	for _, block := range f.Blocks {
		cacheBlockPath := fileCachePath + "/" + strconv.FormatInt(block.BlockSeq, 10)

		_, err := os.Stat(cacheBlockPath)
		if err == nil {
			logrus.Debugf("block exist, block seq: %v", block.BlockSeq)
			continue
		}

		tmp := cacheBlockPath + ".tmp"
		os.Remove(tmp)

		mailText, b, err := mailfs.downloadUID(block.UID)
		if err != nil {
			return err
		}

		md5byte := md5.Sum(b)
		blockmd5 := hex.EncodeToString(md5byte[:])
		if block.BlockMD5 != blockmd5 {
			errStr := "block md5 not match"
			logrus.Errorf(errStr)
			return errors.New(errStr)
		}

		if mailText.Vmailfolder != f.MailFolder {
			errStr := "mailfolder not match"
			logrus.Errorf(errStr)
			return errors.New(errStr)
		}

		if mailText.Vlocalpath != f.LocalPath {
			errStr := "localpath not match"
			logrus.Errorf(errStr)
			return errors.New(errStr)
		}

		logrus.Infof("uid: %v, blockmd5: %v, filesize: %v", block.UID, mailText.Vblockmd5, len(b))

		tmpbf, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			logrus.Errorf("OpenFile %v, err: %v", tmp, err)
			return err
		}

		tmpbf.Write(b)
		tmpbf.Close()

		err = os.Rename(tmp, cacheBlockPath)
		if err != nil {
			logrus.Errorf("rename error %v -> %v, err: %v", tmp, cacheBlockPath, err)
			return err
		}
	}

	tmpf, err := os.OpenFile(saveTmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		logrus.Errorf("OpenFile %v, err: %v", saveTmpPath, err)
		return err
	}

	for _, block := range f.Blocks {
		cacheBlockPath := fileCachePath + "/" + strconv.FormatInt(block.BlockSeq, 10)
		cacheBlock, err := os.OpenFile(cacheBlockPath, os.O_RDONLY, 0644)
		if err != nil {
			logrus.Errorf("OpenFile %v, err: %v", cacheBlockPath, err)
			return err
		}

		blockdata, err := io.ReadAll(cacheBlock)
		cacheBlock.Close()
		if err != nil {
			logrus.Errorf("read file %v error: %v", cacheBlockPath, err)
			return err
		}

		_, err = tmpf.Write(blockdata)
		if err != nil {
			logrus.Errorf("write file %v error: %v", saveTmpPath, err)
			return err
		}
	}

	tmpf.Close()

	filemd5, _ := md5File(saveTmpPath)
	if filemd5 != f.FileMD5 {
		logrus.Errorf("file %v md5 not match, %v wanna(%v) error", saveTmpPath, filemd5, f.FileMD5)
		return errors.New("file md5 not match")
	}

	err = os.Rename(saveTmpPath, savePath)
	if err != nil {
		logrus.Errorf("rename error %v -> %v, err: %v", saveTmpPath, savePath, err)
		return err
	}

	err = os.RemoveAll(fileCachePath)
	if err != nil {
		logrus.Warnf("remove dir %v, err: %v", fileCachePath, err)
	}

	return nil
}

func (mailfs *MailFileSystem) UploadFileEach(header *mail.Header, text []byte, fileName string, block []byte) error {
	return mailfs.withRetry("UploadFileEach/APPEND", func() error {
		var mailBuf bytes.Buffer
		mw, err := mail.CreateWriter(&mailBuf, *header)
		if err != nil {
			return err
		}

		textHeader := mail.InlineHeader{}
		textHeader.Set("Content-Type", "text/plain; charset=utf-8")
		textWriter, err := mw.CreateSingleInline(textHeader)
		if err != nil {
			return err
		}

		textWriter.Write(text)
		textWriter.Close()

		attachHeader := mail.AttachmentHeader{}
		attachHeader.Set("Content-Type", "text/plain")
		attachHeader.SetFilename(fileName)
		ap, err := mw.CreateAttachment(attachHeader)
		if err != nil {
			return err
		}

		ap.Write(block)
		ap.Close()

		mw.Close()

		mailData := mailBuf.Bytes()
		appendCmd := mailfs.c.Append(mailfs.remoteDir, int64(len(mailData)), nil)
		if _, err := appendCmd.Write(mailData); err != nil {
			logrus.Errorf("failed to write message: %v", err)
			return err
		}
		if err := appendCmd.Close(); err != nil {
			logrus.Errorf("failed to close message: %v", err)
		}

		appendData, err := appendCmd.Wait()
		if err != nil {
			return err
		}

		logrus.Printf("邮件上传成功: %v", appendData.UID)
		return nil
	})
}

// GenHeader 生成邮件头
func (mailfs *MailFileSystem) GenHeader(fileName string, fBlockSeq int64, fBlockCount int64, encrypted bool) (*mail.Header, error) {
	header := mail.Header{}
	header.SetAddressList("From", []*mail.Address{{
		Name:    "阳光",
		Address: "1096693846@qq.com",
	}})

	header.SetAddressList("to", []*mail.Address{{
		Name:    "阳光",
		Address: "1096693846@qq.com",
	}})

	mode := "plain"
	subjectName := fileName
	if encrypted {
		mode = "encrypted"
		subjectName = Encrypt(fileName)
	}

	header.SetSubject(fmt.Sprintf("%v/%v/%v-%v", subjectName, mode, fBlockSeq, fBlockCount))
	header.SetDate(time.Now())
	header.SetContentType("multipart/mixed", nil)

	return &header, nil
}

func (mailfs *MailFileSystem) GetMailboxList() ([]string, error) {
	var folders []string

	err := mailfs.withRetry("GetMailboxList/LIST", func() error {
		listCmd := mailfs.c.List("", "其他文件夹/*", nil)
		mboxes, err := listCmd.Collect()
		if err != nil {
			logrus.Errorf("IMAP LIST failed: %v", err)
			return err
		}

		folders = make([]string, 0, len(mboxes))
		for _, mb := range mboxes {
			folders = append(folders, mb.Mailbox)
		}
		return nil
	})

	if err != nil {
		return folders, err
	}

	// 根据配置文件过滤目录列表
	allowed := LoadAllowedFolders()
	folders = FilterFolders(folders, allowed)

	return folders, nil
}

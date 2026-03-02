package libfs

import (
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
	"github.com/sirupsen/logrus"
	"io"
	"io/fs"
	"mime/quotedprintable"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const FileBlockSize = 512 * 65536 //32M

type MailFileSystem struct {
	c               *imapclient.Client
	remoteDir       string
	downloadRootDir string
	db              sql.DB
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

func (mailfs *MailFileSystem) Login(usr string, pwd string) error {
	var err error = nil
	mailfs.c, err = imapclient.DialTLS("imap.qq.com:993", nil)
	if err != nil {
		logrus.Errorf("failed to dial IMAP server: %v", err)
		return err
	}

	if err = mailfs.c.Login(usr, pwd).Wait(); err != nil {
		logrus.Errorf("failed to login: %v", err)
		return err
	}

	return nil
}

func (mailfs *MailFileSystem) Logout() {
	if mailfs.c == nil {
		return
	}
	mailfs.c.Logout()
	mailfs.c = nil
}

func (mailfs *MailFileSystem) cacheUID(uid imap.UID) error {

	logrus.Debugf("cacheUID : %v", uid)

	uidSeqSet := imap.UIDSetNum(uid)
	bodySection := &imap.FetchItemBodySection{Part: []int{1}}
	fetchOptions := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}

	messages, err := mailfs.c.Fetch(uidSeqSet, fetchOptions).Collect()
	if err != nil {
		logrus.Errorf("FETCH command failed: %v", err)
		return err
	}

	if len(messages) <= 0 {
		logrus.Errorf("len(messages) <= 0")
		return errors.New("len(messages) <= 0")
	}

	msg := messages[0]
	header := msg.FindBodySection(bodySection)

	r := quotedprintable.NewReader(bytes.NewReader(header))
	b, err := io.ReadAll(r)
	if err != nil {
		logrus.Errorf("io.ReadAll failed: %v", err)
		return err
	}

	mailText := MailTextFromByte(string(b))

	if len(mailText.Vsubject) == 0 || len(mailText.Vlocalpath) == 0 {
		logrus.Warnf("not a mailfs: %v, uid: ", uid)
		return nil
	}

	err = cacheToDB(msg.UID, mailText)
	if err != nil {
		logrus.Errorf("cacheToDB failed: %v", err)
		return err
	}

	return nil
}

func (mailfs *MailFileSystem) CacheCurrDir() error {
	if mailfs.c == nil {
		return errors.New("not login")
	}

	if len(mailfs.remoteDir) <= 0 {
		return errors.New("not select dir")
	}

	uids, err := mailfs.c.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{},
	}, nil).Wait()
	if err != nil {
		logrus.Errorf("UID SEARCH command failed: %v", err)
	}

	logrus.Infof("UID has: %v", uids.AllUIDs())

	for _, uid := range uids.AllUIDs() {
		err := mailfs.cacheUID(uid)
		if err != nil {
			logrus.Errorf("cacheUID failed: %v", err)
			return err
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

	return getCacheFileFromDB(mailfs.remoteDir)
}

func (mailfs *MailFileSystem) downloadUID(uid int64) (*MailText, []byte, error) {
	logrus.Debugf("downloadUID : %v", uid)

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
		return nil, nil, errors.New("no msg from fetch")
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
		return nil, nil, errors.New("FETCH command did not return body section")
	}

	mr, err := mail.CreateReader(bodyData.Literal)
	if err != nil {
		logrus.Errorf("failed to create mail reader: %v", err)
		return nil, nil, err
	}

	var mailText *MailText
	var fileData []byte

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			logrus.Errorf("failed to read message part: %v", err)
			return nil, nil, err
		}

		switch p.Header.(type) {
		case *mail.InlineHeader:
			b, err := io.ReadAll(p.Body)
			if err != nil {
				logrus.Errorf("failed to get inline: %v", err)
				return nil, nil, err
			}
			mailText = MailTextFromByte(string(b))
		case *mail.AttachmentHeader:
			fileData, err = io.ReadAll(p.Body)
			if err != nil {
				logrus.Errorf("failed to get attachment file: %v", err)
				return nil, nil, err
			}
		}
	}

	if mailText == nil || fileData == nil {
		logrus.Errorf("mailText == nil || fileData == nil")
		return nil, nil, errors.New("parse mail error")
	}

	if len(mailText.Vsubject) == 0 || len(mailText.Vlocalpath) == 0 {
		errStr := fmt.Sprintf("not a mailfs: %v, uid: ", uid)
		logrus.Error(errStr)
		return nil, nil, errors.New(errStr)
	}

	return mailText, fileData, nil
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
			return err
		}

		if mailText.Vmailfolder != f.MailFolder {
			errStr := "mailfolder not match"
			logrus.Errorf(errStr)
			return err
		}

		if mailText.Vlocalpath != f.LocalPath {
			errStr := "localpath not match"
			logrus.Errorf(errStr)
			return err
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
		logrus.Errorf("file %v md5 not match, %v wanna(%v) error %v -> %v", saveTmpPath, filemd5, f.FileMD5)
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
	// 创建邮件缓冲区
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

	// 5. 添加附件（可选）
	// 如果不需要附件，可以省略此部分
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

	// ----- IMAP APPEND -----
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
}

func (mailfs *MailFileSystem) UploadFile(path string) error {
	if mailfs.c == nil {
		return errors.New("not login")
	}

	if len(mailfs.remoteDir) <= 0 {
		return errors.New("not select dir")
	}

	logrus.Infof("remote: %v, upload file: %v", mailfs.remoteDir, path)

	existed, err := isFileExisted(mailfs.remoteDir, path)
	if err != nil {
		logrus.Errorf("isFileExisted occur error: %v", path)
		return err
	}

	if existed {
		logrus.Infof("ignore..., remote: %v, file has existed in mail: %v", mailfs.remoteDir, path)
		return nil
	}

	filemd5, err := md5File(path)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_RDONLY, 0644)

	if err != nil {
		return err
	}

	fInfo, err := f.Stat()
	if err != nil {
		return err
	}

	var fSize int64 = fInfo.Size()
	var fBlockCount int64 = fSize / FileBlockSize
	if fSize%FileBlockSize != 0 {
		fBlockCount++
	}

	fileName := filepath.Base(path)

	fileBlock := make([]byte, FileBlockSize)
	for i := int64(1); i <= fBlockCount; i++ {
		n, err := f.Read(fileBlock)
		if err != nil {
			return err
		}

		if n < FileBlockSize {
			fileBlock = fileBlock[:n]
		}

		md5byte := md5.Sum(fileBlock)
		blockmd5 := hex.EncodeToString(md5byte[:])

		header, err := mailfs.GenHeader(fileName, i, fBlockCount)
		if err != nil {
			return err
		}

		mailText := MailText{
			Vfilemd5:    filemd5,
			Vblockmd5:   blockmd5,
			Vfilesize:   fSize,
			Vblocksize:  int64(len(fileBlock)),
			Vcreatetime: time.Now(),
			Vowner:      "sunshine",
			Vlocalpath:  path,
			Vmailfolder: mailfs.remoteDir,
		}

		mailText.Vsubject, err = header.Subject()
		if err != nil {
			return err
		}

		err = mailfs.UploadFileEach(header, MailTextToByte(&mailText), fileName, fileBlock)
		if err != nil {
			return err
		}
	}

	return nil
}

func (mailfs *MailFileSystem) UploadDir(path string) error {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return err
	}

	if !fileInfo.IsDir() {
		return errors.New("not a dir")
	}

	err = filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.Type().IsRegular() {
			return nil
		}

		// 获取绝对路径
		absPath, err := filepath.Abs(path)
		if err != nil {
			logrus.Errorf("get path error: %v", err)
			return nil
		}

		err = mailfs.UploadFile(filepath.ToSlash(absPath))
		if err != nil {
			logrus.Errorf("UploadFile error: %v", err)
			return nil
		}

		return nil
	})

	if err != err {
		return err
	}

	return nil
}

func (mailfs *MailFileSystem) GenHeader(fileName string, fBlockSeq int64, fBlockCount int64) (*mail.Header, error) {
	header := mail.Header{}
	header.SetAddressList("From", []*mail.Address{{
		Name:    "阳光",
		Address: "1096693846@qq.com",
	}})

	header.SetAddressList("to", []*mail.Address{{
		Name:    "阳光",
		Address: "1096693846@qq.com",
	}})

	header.SetSubject(fmt.Sprintf("%v/%v/%v-%v", fileName, "plain", fBlockSeq, fBlockCount))
	header.SetDate(time.Now())
	header.SetContentType("multipart/mixed", nil)

	return &header, nil
}

func (mailfs *MailFileSystem) GetMailboxList() ([]string, error) {
	listCmd := mailfs.c.List("", "其他文件夹/*", nil)
	mboxes, err := listCmd.Collect()
	if err != nil {
		logrus.Errorf("IMAP LIST failed: %v", err)
		return nil, err
	}

	folders := make([]string, 0, len(mboxes))
	for _, mb := range mboxes {
		folders = append(folders, mb.Mailbox)
	}
	return folders, nil
}

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
	"time"
)

const FileBlockSize = 512 * 65536 //32M

type MailFileSystem struct {
	c         *imapclient.Client
	remoteDir string
	db        sql.DB
}

func (mailfs *MailFileSystem) Login(usr string, pwd string) error {
	var err error = nil
	mailfs.c, err = imapclient.DialTLS("imap.qq.com:993", nil)
	if err != nil {
		logrus.Fatalf("failed to dial IMAP server: %v", err)
		return err
	}

	if err = mailfs.c.Login(usr, pwd).Wait(); err != nil {
		logrus.Fatalf("failed to login: %v", err)
		return err
	}

	return nil
}

func (mailfs *MailFileSystem) CacheUID(uid imap.UID) error {

	logrus.Debugf("CacheUID : %v", uid)

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
		err := mailfs.CacheUID(uid)
		if err != nil {
			logrus.Errorf("CacheUID failed: %v", err)
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
	logrus.Printf("%v contains %v messages", remoteDir, selectedMbox.NumMessages)
	return nil
}

func (mailfs *MailFileSystem) UploadFileEach(header *mail.Header, text []byte, fileName string, block []byte) error {
	// 创建邮件缓冲区
	var mailBuf bytes.Buffer
	mw, err := mail.CreateWriter(&mailBuf, *header)
	if err != nil {
		return err
	}
	defer mw.Close()

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

	return &header, nil
}

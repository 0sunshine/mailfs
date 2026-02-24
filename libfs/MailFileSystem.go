package libfs

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
	"github.com/sirupsen/logrus"
	"io"
	"log"
	"mime/quotedprintable"
	"os"
	"path/filepath"
	"time"
)

const FileBlockSize = 512 * 65536 //32M

type MailFileSystem struct {
	c         *imapclient.Client
	remoteDir string
}

func (fs *MailFileSystem) Login(usr string, pwd string) error {
	var err error = nil
	fs.c, err = imapclient.DialTLS("imap.qq.com:993", nil)
	if err != nil {
		logrus.Fatalf("failed to dial IMAP server: %v", err)
		return err
	}

	if err = fs.c.Login(usr, pwd).Wait(); err != nil {
		logrus.Fatalf("failed to login: %v", err)
		return err
	}

	return nil
}

func (fs *MailFileSystem) CacheUID(uid imap.UID) error {

	logrus.Debugf("CacheUID : %v", uid)

	uidSeqSet := imap.UIDSetNum(uid)
	bodySection := &imap.FetchItemBodySection{Part: []int{1}}
	fetchOptions := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}

	messages, err := fs.c.Fetch(uidSeqSet, fetchOptions).Collect()
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

	log.Printf("Header:\n%v", string(b))

	return nil
}

func (fs *MailFileSystem) CacheCurrDir() error {
	if fs.c == nil {
		return errors.New("not login")
	}

	if len(fs.remoteDir) <= 0 {
		return errors.New("not select dir")
	}

	uids, err := fs.c.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{},
	}, nil).Wait()
	if err != nil {
		logrus.Fatalf("UID SEARCH command failed: %v", err)
	}

	logrus.Infof("UID has: %v", uids.AllUIDs())

	for _, uid := range uids.AllUIDs() {
		err := fs.CacheUID(uid)
		if err != nil {
			logrus.Fatalf("CacheUID failed: %v", err)
			return err
		}
	}

	return nil
}

func (fs *MailFileSystem) Enter(remoteDir string) error {
	if fs.c == nil {
		return errors.New("not login")
	}

	selectedMbox, err := fs.c.Select(remoteDir, nil).Wait()
	if err != nil {
		logrus.Fatalf("failed to select : %v", remoteDir)
		return err
	}

	fs.remoteDir = remoteDir
	logrus.Printf("%v contains %v messages", remoteDir, selectedMbox.NumMessages)
	return nil
}

func (fs *MailFileSystem) UploadFileEach(header *mail.Header, text []byte, fileName string, block []byte) error {
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
	appendCmd := fs.c.Append(fs.remoteDir, int64(len(mailData)), nil)
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

func (fs *MailFileSystem) UploadFile(path string) error {
	if fs.c == nil {
		return errors.New("not login")
	}

	if len(fs.remoteDir) <= 0 {
		return errors.New("not select dir")
	}

	filemd5, err := md5File(path)
	if err != nil {
		return err
	}

	logrus.Printf("filemd5: %v", filemd5)

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

		mailText := MailText{
			filemd5:    filemd5,
			blockmd5:   blockmd5,
			filesize:   fSize,
			blocksize:  int64(len(fileBlock)),
			createtime: time.Now(),
			owner:      "sunshine",
			localpath:  path,
			mailfolder: fs.remoteDir,
		}

		header, err := fs.GenHeader(fileName, i, fBlockCount)
		if err != nil {
			return err
		}

		err = fs.UploadFileEach(header, MailTextToByte(&mailText), fileName, fileBlock)
		if err != nil {
			return err
		}
	}

	return nil
}

func (fs *MailFileSystem) GenHeader(fileName string, fBlockSeq int64, fBlockCount int64) (*mail.Header, error) {
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

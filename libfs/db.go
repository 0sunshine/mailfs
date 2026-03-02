package libfs

import (
	"database/sql"
	_ "database/sql"
	"errors"
	"github.com/emersion/go-imap/v2"
	"github.com/sirupsen/logrus"
	_ "github.com/xeodou/go-sqlcipher" // 导入加密驱动
	"strconv"
	"strings"
)

var db *sql.DB

func init() {
	if err := dbOpen(); err != nil {
		logrus.Errorf("dbOpen error: %v", err)
	}
}

func dbOpen() error {
	// 数据库文件名
	dbPath := "my_encrypted.db"
	// 你的密码
	//password := "your-strong-password"

	//dsn := dbPath + "?_key=" + password

	dsn := dbPath

	var err error

	db, err = sql.Open("sqlite3", dsn)
	if err != nil {
		logrus.Errorf("sql open error")
		return err
	}

	if err = db.Ping(); err != nil {
		logrus.Errorf("sql ping error: %v", err)
		return err
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS cache_files (
    												fileid INTEGER PRIMARY KEY AUTOINCREMENT,
													mailfolder VARCHAR(256) NOT NULL,
    												localpath VARCHAR(512) NOT NULL,
													blockcount INTEGER NOT NULL,
    												filemd5 VARCHAR(64) NOT NULL,
													UNIQUE(mailfolder, localpath)
												);
	`)
	if err != nil {
		logrus.Errorf("create table cache_files error")
		return err
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS cache_blocks (
    												fileid INTEGER,
													blockseq INTEGER NOT NULL,
													uid INTEGER NOT NULL,
													blockmd5 VARCHAR(64) NOT NULL,
    												UNIQUE(fileid, blockseq)
												);
`)
	if err != nil {
		logrus.Errorf("create table cache_blocks error")
		return err
	}

	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_cache_blocks_uid ON cache_blocks(uid);`)
	if err != nil {
		logrus.Errorf("create index idx_cache_blocks_uid error")
		return err
	}

	return nil
}

func cacheToDB(uid imap.UID, m *MailText) error {

	s := strings.Split(m.Vsubject, "/")
	if len(s) < 3 {
		logrus.Errorf("subject error")
		return errors.New("subject error")
	}

	n := strings.Split(s[2], "-")
	if len(s) < 2 {
		logrus.Errorf("subject seq error")
		return errors.New("subject seq error")
	}

	blockseq, err := strconv.Atoi(n[0])
	if err != nil {
		logrus.Errorf("get blockseq error")
		return err
	}

	blockcount, err := strconv.Atoi(n[1])
	if err != nil {
		logrus.Errorf("get blockseq error")
		return err
	}

	var fileid int64

	rows, err := db.Query(`SELECT fileid FROM cache_files WHERE mailfolder=? AND localpath=?;`, m.Vmailfolder, m.Vlocalpath)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return err
	}

	if rows.Next() {
		if err := rows.Scan(&fileid); err != nil {
			logrus.Errorf("rows.Scan(&fileid) error: %v", err)
			return err
		}
	}
	rows.Close()

	if fileid <= 0 {
		r, err := db.Exec(`INSERT INTO cache_files (mailfolder, localpath, blockcount, filemd5) 
										  VALUES (?,?,?,?);`, m.Vmailfolder, m.Vlocalpath, blockcount, m.Vfilemd5)
		if err != nil {
			logrus.Errorf("sql error: %v", err)
			return err
		}

		fileid, err = r.LastInsertId()
		if err != nil {
			logrus.Errorf("LastInsertId error: %v", err)
			return err
		}
	}

	_, err = db.Exec(`INSERT OR REPLACE INTO cache_blocks (fileid, blockseq, uid, blockmd5) 
										  VALUES (?,?,?,?);`, fileid, blockseq, int32(uid), m.Vblockmd5)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return err
	}

	return nil
}

func isFileExisted(remoteDir string, localpath string) (bool, error) {
	rows, err := db.Query(`SELECT fileid FROM cache_files WHERE mailfolder=? AND localpath=?;`, remoteDir, localpath)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return false, err
	}
	defer rows.Close()

	if rows.Next() {
		return true, nil
	}

	return false, nil
}

func getCacheFileFromDB(remoteDir string) ([]CacheFile, error) {
	rows, err := db.Query(`SELECT * FROM cache_files WHERE mailfolder=?;`, remoteDir)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return nil, err
	}
	defer rows.Close()

	files := make([]CacheFile, 0, 300000)

	for rows.Next() {
		f := CacheFile{}
		if err := rows.Scan(&f.FileID, &f.MailFolder, &f.LocalPath, &f.BlockCount, &f.FileMD5); err != nil {
			logrus.Errorf("rows.Scan error: %v", err)
			return nil, err
		}

		files = append(files, f)
	}

	return files, nil
}

func getCacheBlockFromDB(fileid int64) ([]CacheBlock, error) {
	rows, err := db.Query(`SELECT * FROM cache_blocks WHERE fileid=?;`, fileid)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return nil, err
	}
	defer rows.Close()

	blocks := make([]CacheBlock, 0, 10)
	for rows.Next() {
		b := CacheBlock{}
		if err := rows.Scan(&b.FileID, &b.BlockSeq, &b.UID, &b.BlockMD5); err != nil {
			logrus.Errorf("rows.Scan error: %v", err)
			return nil, err
		}

		blocks = append(blocks, b)
	}

	return blocks, nil
}

func isUIDCached(remoteDir string, uid int64) (bool, error) {
	rows, err := db.Query(`SELECT * FROM cache_blocks a INNER JOIN cache_files b 
         ON a.fileid=b.fileid WHERE a.uid=? and b.mailfolder=?;`, uid, remoteDir)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return false, err
	}
	defer rows.Close()

	if rows.Next() {
		return true, nil
	}

	return false, nil
}

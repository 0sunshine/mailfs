package libfs

import (
	"database/sql"
	_ "database/sql"
	"errors"
	"github.com/emersion/go-imap/v2"
	"github.com/sirupsen/logrus"
	_ "github.com/xeodou/go-sqlcipher"
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
	dbPath := "my_encrypted.db"
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
		filesize INTEGER NOT NULL DEFAULT 0,
		UNIQUE(mailfolder, localpath)
	);`)
	if err != nil {
		logrus.Errorf("create table cache_files error")
		return err
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS cache_blocks (
		fileid INTEGER,
		blockseq INTEGER NOT NULL,
		uid INTEGER NOT NULL,
		blockmd5 VARCHAR(64) NOT NULL,
		blocksize INTEGER NOT NULL DEFAULT 0,
		UNIQUE(fileid, blockseq)
	);`)
	if err != nil {
		logrus.Errorf("create table cache_blocks error")
		return err
	}

	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_cache_blocks_uid ON cache_blocks(uid);`)
	if err != nil {
		logrus.Errorf("create index idx_cache_blocks_uid error")
		return err
	}

	// ── 自动迁移：为已有表添加新字段（忽略"已存在"错误）──
	db.Exec(`ALTER TABLE cache_files ADD COLUMN filesize INTEGER NOT NULL DEFAULT 0;`)
	db.Exec(`ALTER TABLE cache_blocks ADD COLUMN blocksize INTEGER NOT NULL DEFAULT 0;`)

	return nil
}

func cacheToDB(uid imap.UID, m *MailText) error {

	s := strings.Split(m.Vsubject, "/")
	if len(s) < 3 {
		logrus.Errorf("subject error")
		return errors.New("subject error")
	}

	n := strings.Split(s[2], "-")
	if len(n) < 2 {
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
		logrus.Errorf("get blockcount error")
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
		r, err := db.Exec(`INSERT INTO cache_files (mailfolder, localpath, blockcount, filemd5, filesize) 
										  VALUES (?,?,?,?,?);`, m.Vmailfolder, m.Vlocalpath, blockcount, m.Vfilemd5, m.Vfilesize)
		if err != nil {
			logrus.Errorf("sql error: %v", err)
			return err
		}

		fileid, err = r.LastInsertId()
		if err != nil {
			logrus.Errorf("LastInsertId error: %v", err)
			return err
		}
	} else {
		// 更新 filesize（兼容旧数据）
		if m.Vfilesize > 0 {
			db.Exec(`UPDATE cache_files SET filesize=? WHERE fileid=? AND filesize=0;`, m.Vfilesize, fileid)
		}
	}

	_, err = db.Exec(`INSERT OR REPLACE INTO cache_blocks (fileid, blockseq, uid, blockmd5, blocksize) 
										  VALUES (?,?,?,?,?);`, fileid, blockseq, int32(uid), m.Vblockmd5, m.Vblocksize)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return err
	}

	return nil
}

func getCacheFileFromDB(remoteDir string, localPath string) ([]CacheFile, error) {

	conds := []sqlCondition{
		{"mailfolder", remoteDir, "="},
	}

	expectFileNums := 300000

	if len(localPath) > 0 {
		conds = append(conds,
			sqlCondition{"localpath", localPath, "="},
		)
		expectFileNums = 1
	}

	query, args := sqlBuildQuery("cache_files", conds)
	rows, err := db.Query(query, args...)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return nil, err
	}
	defer rows.Close()

	files := make([]CacheFile, 0, expectFileNums)

	for rows.Next() {
		f := CacheFile{}
		if err := rows.Scan(&f.FileID, &f.MailFolder, &f.LocalPath, &f.BlockCount, &f.FileMD5, &f.FileSize); err != nil {
			logrus.Errorf("rows.Scan error: %v", err)
			return nil, err
		}
		files = append(files, f)
	}

	return files, nil
}

func getCacheBlockFromDB(fileid int64) ([]CacheBlock, error) {
	rows, err := db.Query(`SELECT fileid, blockseq, uid, blockmd5, blocksize FROM cache_blocks WHERE fileid=?;`, fileid)
	if err != nil {
		logrus.Errorf("sql error: %v", err)
		return nil, err
	}
	defer rows.Close()

	blocks := make([]CacheBlock, 0, 10)
	for rows.Next() {
		b := CacheBlock{}
		if err := rows.Scan(&b.FileID, &b.BlockSeq, &b.UID, &b.BlockMD5, &b.BlockSize); err != nil {
			logrus.Errorf("rows.Scan error: %v", err)
			return nil, err
		}
		blocks = append(blocks, b)
	}

	return blocks, nil
}

func isUIDCached(remoteDir string, uid int64) (bool, error) {
	rows, err := db.Query(`SELECT 1 FROM cache_blocks a INNER JOIN cache_files b 
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

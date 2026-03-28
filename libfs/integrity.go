package libfs

// ──────────────────────────────────────────────────────────────────────────────
// CheckIntegrity — 检查指定文件夹下所有文件的块完整性
// ──────────────────────────────────────────────────────────────────────────────

type IntegrityResult struct {
	File           CacheFile
	CachedBlocks   int64
	ExpectedBlocks int64
	OK             bool
}

func CheckIntegrity(folder string) ([]IntegrityResult, error) {
	files, err := getCacheFileFromDB(folder, "")
	if err != nil {
		return nil, err
	}

	results := make([]IntegrityResult, 0, len(files))
	for _, f := range files {
		blocks, err := getCacheBlockFromDB(f.FileID)
		if err != nil {
			return nil, err
		}
		r := IntegrityResult{
			File:           f,
			CachedBlocks:   int64(len(blocks)),
			ExpectedBlocks: f.BlockCount,
			OK:             int64(len(blocks)) == f.BlockCount,
		}
		results = append(results, r)
	}
	return results, nil
}

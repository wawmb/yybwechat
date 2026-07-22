package httpapi

import "path/filepath"

// DefaultDBFilename 是 SQLite 数据库文件的默认名称。
const DefaultDBFilename = "yyb.db"

// prepareDBPath 根据数据库目录和文件名拼接完整的数据库文件路径。
func prepareDBPath(dbDir, filename string) (string, error) {
	if filename == "" {
		filename = DefaultDBFilename
	}
	return filepath.Join(dbDir, filename), nil
}

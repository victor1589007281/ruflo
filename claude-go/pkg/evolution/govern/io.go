package govern

import (
	"io"
	"os"
)

// readHead 读文件前 n 字节 (只需要 frontmatter, 不必把整个技能正文读进内存)。
func readHead(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(buf[:read]), nil
}

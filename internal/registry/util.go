package registry

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// getFreePort 获取本地空闲端口
func getFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitPortListening 等待端口监听成功（超时5秒）
func waitPortListening(host string, port int) error {
	addr := net.JoinHostPort(host, string(rune(port)))
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("port %d not listening after 5s", port)
		case <-ticker.C:
			conn, err := net.Dial("tcp", addr)
			if err == nil {
				conn.Close()
				return nil
			}
		}
	}
}

// genWorkerdEnv 生成 workerd 环境变量配置（Cap'n Proto 格式）
func genWorkerdEnv(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	var envLines []string
	for k, v := range env {
		envLines = append(envLines, fmt.Sprintf(`(name = "%s", value = "%s")`, k, v))
	}
	return strings.Join(envLines, ",\n          ")
}

// getStorageDir 获取存储目录（配置/代码/日志）
func getStorageDir() string {
	dir := filepath.Join(os.TempDir(), "faas-workerd-storage")
	os.MkdirAll(dir, 0755) // 自动创建目录
	return dir
}

package util

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// GetFreePort 获取本地空闲端口
func GetFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// WaitPortListening 等待端口监听成功（超时5秒）
func WaitPortListening(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
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

// GetStorageDir 获取存储目录（配置/代码/日志）
func GetStorageDir() string {
	dir := filepath.Join("faas-workerd-storage")
	os.MkdirAll(dir, 0755) // 自动创建目录
	return dir
}

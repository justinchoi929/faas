package registry

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// WorkerdConfig workerd 进程配置
type WorkerdConfig struct {
	Port     int    // 监听端口
	ConfPath string // 配置文件路径
	CodePath string // 函数代码文件路径
	LogPath  string // 日志文件路径
	Pid      int    // 进程ID
}

// FunctionMetadata 函数元数据
type FunctionMetadata struct {
	Name      string            // 函数名（唯一）
	Subdomain string            // 子域名（如 foo.func.local）
	Runtime   string            // 运行时（仅支持 js）
	Code      string            // 函数代码（JS 源码）
	EnvVars   map[string]string // 环境变量
	Version   string            // 版本（可选）
	Alias     string            // 别名（可选）
	CreatedAt time.Time         // 创建时间
	UpdatedAt time.Time         // 更新时间
	Workerd   WorkerdConfig     // workerd 配置
}

// Registry 函数注册表（单例）
type Registry struct {
	funcs        map[string]*FunctionMetadata // 函数名 -> 元数据
	subdomainMap map[string]string            // 子域名 -> 函数名
	mu           sync.RWMutex                 // 并发安全锁
	StorageDir   string                       // 存储目录
	workerdBin   string                       // workerd 二进制路径
}

var defaultRegistry *Registry

// Default 获取单例注册表
func Default(workerdBin string) *Registry {
	if defaultRegistry == nil {
		defaultRegistry = &Registry{
			funcs:        make(map[string]*FunctionMetadata),
			subdomainMap: make(map[string]string),
			StorageDir:   getStorageDir(),
			workerdBin:   workerdBin,
		}
	}
	return defaultRegistry
}

// --------------- 核心方法：生成 workerd 配置与代码文件 ---------------
func (r *Registry) generateWorkerdFiles(meta *FunctionMetadata) error {
	// 1. 生成函数代码文件（如 storage/foo.js）
	codeFile := fmt.Sprintf("%s.js", meta.Name)
	codePath := filepath.Join(r.StorageDir, codeFile)
	if err := os.WriteFile(codePath, []byte(meta.Code), 0644); err != nil {
		return fmt.Errorf("write code: %w", err)
	}
	meta.Workerd.CodePath = codePath

	// 2. 生成配置文件（注意 embed 必须是相对路径）
	confPath := filepath.Join(r.StorageDir, fmt.Sprintf("%s.capnp", meta.Name))
	confContent := fmt.Sprintf(`
using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (
      name = "%s",
      worker = (
        serviceWorkerScript = embed "%s",
        compatibilityDate = "2024-05-01"
      )
    )
  ],
  sockets = [
    (
      name = "http",
      address = "127.0.0.1:%d",
      http = (),
      service = "%s"
    )
  ]
);
`, meta.Name, codeFile, meta.Workerd.Port, meta.Name)

	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		return fmt.Errorf("write conf: %w", err)
	}
	meta.Workerd.ConfPath = confPath

	// 3. 生成日志文件路径
	logPath := filepath.Join(r.StorageDir, fmt.Sprintf("%s.log", meta.Name))
	meta.Workerd.LogPath = logPath
	return nil
}

// --------------- 核心方法：启动/停止 workerd 进程 ---------------
func (r *Registry) startWorkerd(meta *FunctionMetadata) error {
	// 生成配置/代码文件
	if err := r.generateWorkerdFiles(meta); err != nil {
		return err
	}

	// 启动 workerd 进程（命令：workerd serve 配置文件）
	cmd := exec.Command(r.workerdBin, "serve", meta.Workerd.ConfPath)
	fmt.Printf("[DEBUG] Running: %s serve %s\n", r.workerdBin, meta.Workerd.ConfPath)
	// 重定向日志到文件
	logFile, err := os.OpenFile(meta.Workerd.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// 启动进程并记录 PID
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start workerd: %w", err)
	}
	meta.Workerd.Pid = cmd.Process.Pid

	// 等待端口监听成功
	if err := waitPortListening("127.0.0.1", meta.Workerd.Port); err != nil {
		cmd.Process.Kill() // 启动失败，清理进程
		return fmt.Errorf("wait port: %w", err)
	}
	return nil
}

func (r *Registry) stopWorkerd(meta *FunctionMetadata) error {
	if meta.Workerd.Pid == 0 {
		return nil // 进程未启动
	}

	// 查找进程并发送终止信号
	process, err := os.FindProcess(meta.Workerd.Pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send signal: %w", err)
	}
	// 等待进程退出
	_, err = process.Wait()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("wait exit: %w", err)
	}
	return nil
}

// --------------- 核心方法：注册/更新函数 ---------------
func (r *Registry) RegisterOrUpdate(meta *FunctionMetadata) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 1. 分配空闲端口
	freePort, err := getFreePort()
	if err != nil {
		return fmt.Errorf("get free port: %w", err)
	}
	meta.Workerd.Port = freePort

	// 2. 停止旧函数（更新场景）
	if oldMeta, exists := r.funcs[meta.Name]; exists {
		if err := r.stopWorkerd(oldMeta); err != nil {
			return fmt.Errorf("stop old function: %w", err)
		}
		delete(r.subdomainMap, oldMeta.Subdomain)
	}

	// 3. 启动新函数
	if err := r.startWorkerd(meta); err != nil {
		return fmt.Errorf("start new function: %w", err)
	}

	// 4. 存储元数据
	now := time.Now()
	if _, exists := r.funcs[meta.Name]; !exists {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	r.funcs[meta.Name] = meta
	r.subdomainMap[meta.Subdomain] = meta.Name

	return nil
}

// --------------- 辅助方法：查询函数 ---------------
func (r *Registry) GetBySubdomain(subdomain string) (*FunctionMetadata, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	funcName, exists := r.subdomainMap[subdomain]
	if !exists {
		return nil, false
	}
	meta, exists := r.funcs[funcName]
	return meta, exists
}

func (r *Registry) GetByName(funcName string) (*FunctionMetadata, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	meta, exists := r.funcs[funcName]
	return meta, exists
}

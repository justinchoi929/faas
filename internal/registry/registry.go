package registry

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// WorkerdConfig workerd 进程配置
type WorkerdConfig struct {
	Port     int    `json:"port"`
	ConfPath string `json:"conf_path"`
	CodePath string `json:"code_path"`
	LogPath  string `json:"log_path"`
	Pid      int    `gorm:"-" json:"pid"` // 忽略PID，不持久化
}

// FunctionMetadata 函数元数据
type FunctionMetadata struct {
	gorm.Model               // 内置字段：ID, CreatedAt, UpdatedAt, DeletedAt
	Name       string        `gorm:"uniqueIndex;not null" json:"name"` // 函数名（唯一）
	Subdomain  string        `gorm:"uniqueIndex;not null" json:"subdomain"`
	Runtime    string        `gorm:"not null" json:"runtime"`
	Code       string        `gorm:"type:text;not null" json:"code"`         // 存储函数代码
	EnvVars    JSONMap       `gorm:"type:text;default:'{}'" json:"env_vars"` // 环境变量（JSON存储）
	Version    string        `json:"version"`
	Alias      string        `json:"alias"`
	Workerd    WorkerdConfig `gorm:"type:json;default:'{}'" json:"workerd"` // 嵌套结构体，会被展开为WorkerdPort, WorkerdConfPath等字段
}

// Registry 函数注册表（单例）
type Registry struct {
	funcs        map[string]*FunctionMetadata // 函数名 -> 元数据
	subdomainMap map[string]string            // 子域名 -> 函数名
	mu           sync.RWMutex                 // 并发安全锁
	StorageDir   string                       // 存储目录
	workerdBin   string                       // workerd 二进制路径
	db           *gorm.DB                     // 数据库连接
}

var defaultRegistry *Registry

// Default 获取单例注册表
func Default(workerdBin string) *Registry {
	if defaultRegistry == nil {
		// 初始化数据库
		db, err := gorm.Open(sqlite.Open(filepath.Join(getStorageDir(), "faas.db")), &gorm.Config{})
		if err != nil {
			panic(fmt.Sprintf("failed to connect database: %v", err))
		}

		// 自动迁移表结构
		if err := db.AutoMigrate(&FunctionMetadata{}); err != nil {
			panic(fmt.Sprintf("failed to migrate database: %v", err))
		}

		// 创建注册表实例
		defaultRegistry = &Registry{
			funcs:        make(map[string]*FunctionMetadata),
			subdomainMap: make(map[string]string),
			StorageDir:   getStorageDir(),
			workerdBin:   workerdBin,
			db:           db,
		}

		// 从数据库加载已保存的函数
		defaultRegistry.loadFromDB()
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
	// 捕获 stderr
	var stderrBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, logFile)
	cmd.Stderr = io.MultiWriter(os.Stderr, logFile)

	// 启动进程并记录 PID
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start workerd: %v, stderr: %s", err, stderrBuf.String())
	}
	meta.Workerd.Pid = cmd.Process.Pid

	// 等待端口监听成功
	if err := waitPortListening("127.0.0.1", meta.Workerd.Port); err != nil {
		cmd.Process.Kill() // 启动失败，清理进程
		return fmt.Errorf("wait port: %v, stderr: %s", err, stderrBuf.String())
	}
	return nil
}

func (r *Registry) stopWorkerd(meta *FunctionMetadata) error {
	if meta.Workerd.Pid == 0 {
		return nil // 进程未启动
	}

	process, err := os.FindProcess(meta.Workerd.Pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}

	// 检查进程是否还活着
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			// 进程已经结束，不需要停止
			meta.Workerd.Pid = 0
			return nil
		}
		return fmt.Errorf("check process: %w", err)
	}

	// 发送终止信号
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			// 已经结束
			meta.Workerd.Pid = 0
			return nil
		}
		return fmt.Errorf("send signal: %w", err)
	}

	// 等待进程退出
	_, err = process.Wait()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("wait exit: %w", err)
	}

	meta.Workerd.Pid = 0
	return nil
}

// --------------- 核心方法：注册/更新函数 ---------------
func (r *Registry) RegisterOrUpdate(meta *FunctionMetadata) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 分配空闲端口
	freePort, err := getFreePort()
	if err != nil {
		return fmt.Errorf("get free port: %w", err)
	}
	meta.Workerd.Port = freePort

	// 停止旧函数（更新场景）
	if oldMeta, exists := r.funcs[meta.Name]; exists {
		if err := r.stopWorkerd(oldMeta); err != nil {
			return fmt.Errorf("stop old function: %w", err)
		}
		delete(r.subdomainMap, oldMeta.Subdomain)
		// 保留旧记录主键和域名
		meta.ID = oldMeta.ID
		meta.CreatedAt = oldMeta.CreatedAt
		meta.Subdomain = oldMeta.Subdomain
	}

	// 启动新函数
	if err := r.startWorkerd(meta); err != nil {
		return fmt.Errorf("start new function: %w", err)
	}

	// 存储元数据
	now := time.Now()
	if _, exists := r.funcs[meta.Name]; !exists {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	if err := r.db.Save(meta).Error; err != nil { // gorm.Save会自动判断新增/更新
		return fmt.Errorf("save to db: %w", err)
	}

	// 更新内存映射
	r.funcs[meta.Name] = meta
	r.subdomainMap[meta.Subdomain] = meta.Name

	return nil
}

// 从数据库加载函数元数据并启动进程
func (r *Registry) loadFromDB() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var metas []*FunctionMetadata
	if err := r.db.Find(&metas).Error; err != nil {
		return fmt.Errorf("load from db: %w", err)
	}

	for _, meta := range metas {
		// 重置端口（避免重启后端口冲突）
		meta.Workerd.Port = 0
		// 重新生成配置文件并启动进程
		if err := r.startWorkerd(meta); err != nil {
			// 记录启动失败的函数，但继续加载其他函数
			fmt.Printf("failed to restart function %s: %v\n", meta.Name, err)
			continue
		}
		// 加入内存映射
		r.funcs[meta.Name] = meta
		r.subdomainMap[meta.Subdomain] = meta.Name
	}

	fmt.Printf("loaded %d functions from database\n", len(r.funcs))
	return nil
}

// 删除函数（内存+数据库）
func (r *Registry) DeleteFunction(funcName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	meta, exists := r.funcs[funcName]
	if !exists {
		return errors.New("function not found")
	}

	// 停止进程
	if err := r.stopWorkerd(meta); err != nil {
		return fmt.Errorf("stop function: %w", err)
	}

	// 从数据库删除
	if err := r.db.Delete(meta).Error; err != nil {
		return fmt.Errorf("delete from db: %w", err)
	}

	// 从内存删除
	delete(r.funcs, funcName)
	delete(r.subdomainMap, meta.Subdomain)

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

// 实现gorm.Valuer接口，将WorkerdConfig转换为JSON字符串
func (w WorkerdConfig) Value() (driver.Value, error) {
	return json.Marshal(w)
}

// 实现gorm.Scanner接口，从JSON字符串解析为WorkerdConfig
func (w *WorkerdConfig) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return errors.New("type assertion to []byte failed")
	}
	return json.Unmarshal(b, &w)
}

// 自定义 JSON 类型做映射
type JSONMap map[string]string

func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return "{}", nil
	}
	return json.Marshal(m)
}

func (m *JSONMap) Scan(value interface{}) error {
	if value == nil {
		*m = make(JSONMap)
		return nil
	}
	var data []byte
	switch v := value.(type) {
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		return fmt.Errorf("unsupported type: %T", value)
	}
	return json.Unmarshal(data, m)
}
